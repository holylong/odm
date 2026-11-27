#include "net.h"
#include "util.h"
#include <ws2tcpip.h>
#include <algorithm>
#include <cstring>
#include <stdexcept>

namespace odm {
namespace {
struct Wsa {
    Wsa() {
        WSADATA data{};
        check(WSAStartup(MAKEWORD(2, 2), &data) == 0, "Winsock init failed");
    }
    ~Wsa() { WSACleanup(); }
};
void ensureWsa() {
    static Wsa wsa;
    (void)wsa;
}

std::wstring lower(std::wstring text) {
    for (auto& c : text)
        if (c >= L'A' && c <= L'Z') c = wchar_t(c - L'A' + L'a');
    return text;
}

bool covers(const std::wstring& pattern, const std::wstring& host) {
    const auto name = lower(pattern);
    const auto name2 = lower(host);
    if (name == name2) return true;
    if (name.size() > 2 && name[0] == L'*' && name[1] == L'.') {
        const auto suffix = name.substr(1);
        return name2.size() > suffix.size() && name2.compare(name2.size() - suffix.size(), suffix.size(), suffix) == 0;
    }
    return false;
}
}

Tcp Tcp::dial(const std::wstring& host, unsigned short port, int timeoutMs) {
    ensureWsa();
    addrinfoW hints{};
    hints.ai_family = AF_UNSPEC;
    hints.ai_socktype = SOCK_STREAM;
    hints.ai_protocol = IPPROTO_TCP;
    addrinfoW* found = nullptr;
    const auto service = std::to_wstring(port);
    check(GetAddrInfoW(host.c_str(), service.c_str(), &hints, &found) == 0, "Cannot resolve the server host");
    struct Free { addrinfoW* value; ~Free() { FreeAddrInfoW(value); } } guard{found};
    const char* last = "Cannot connect to the server";
    for (auto* it = found; it; it = it->ai_next) {
        Tcp socket(::socket(it->ai_family, it->ai_socktype, it->ai_protocol));
        if (socket.value == INVALID_SOCKET) continue;
        u_long nonBlocking = 1;
        ioctlsocket(socket.value, FIONBIO, &nonBlocking);
        if (connect(socket.value, it->ai_addr, int(it->ai_addrlen)) == SOCKET_ERROR) {
            if (WSAGetLastError() != WSAEWOULDBLOCK) { last = "Cannot connect to the server"; continue; }
            fd_set writable{}, broken{};
            FD_ZERO(&writable);
            FD_SET(socket.value, &writable);
            FD_ZERO(&broken);
            FD_SET(socket.value, &broken);
            timeval tv{timeoutMs / 1000, (timeoutMs % 1000) * 1000};
            const int ready = select(0, nullptr, &writable, &broken, &tv);
            if (ready <= 0 || FD_ISSET(socket.value, &broken)) { last = "Timed out connecting to the server"; continue; }
        }
        nonBlocking = 0;
        ioctlsocket(socket.value, FIONBIO, &nonBlocking);
        const DWORD timeout = DWORD(timeoutMs);
        setsockopt(socket.value, SOL_SOCKET, SO_RCVTIMEO, reinterpret_cast<const char*>(&timeout), sizeof(timeout));
        setsockopt(socket.value, SOL_SOCKET, SO_SNDTIMEO, reinterpret_cast<const char*>(&timeout), sizeof(timeout));
        return socket;
    }
    throw std::runtime_error(last);
}

void Tcp::send(const std::string& data) {
    size_t offset = 0;
    while (offset < data.size()) {
        const int n = ::send(value, data.data() + offset, int(data.size() - offset), 0);
        check(n > 0, "Lost the network connection");
        offset += size_t(n);
    }
}

int Tcp::recv(char* data, int size) {
    const int n = ::recv(value, data, size, 0);
    check(n >= 0, "Network read failed; retry to resume");
    return n;
}

void Tcp::close() {
    if (value != INVALID_SOCKET) {
        closesocket(value);
        value = INVALID_SOCKET;
    }
}

Tls::Tls(Tcp raw, const std::wstring& host) : raw(std::move(raw)) { handshake(host); }

Tls::~Tls() { close(); }

void Tls::handshake(const std::wstring& host) {
    SCHANNEL_CRED sc{};
    sc.dwVersion = SCHANNEL_CRED_VERSION;
    sc.dwFlags = SCH_CRED_MANUAL_CRED_VALIDATION | SCH_CRED_NO_SERVERNAME_CHECK | SCH_USE_STRONG_CRYPTO;
    TimeStamp expiry{};
    check(AcquireCredentialsHandleW(nullptr, const_cast<SEC_WCHAR*>(UNISP_NAME_W), SECPKG_CRED_OUTBOUND, nullptr, &sc,
                                    nullptr, nullptr, &credential, &expiry) == SEC_E_OK,
          "Cannot initialise TLS");
    haveCredential = true;
    DWORD flags = ISC_REQ_SEQUENCE_DETECT | ISC_REQ_REPLAY_DETECT | ISC_REQ_CONFIDENTIALITY | ISC_REQ_EXTENDED_ERROR |
                  ISC_REQ_STREAM | ISC_REQ_ALLOCATE_MEMORY | ISC_REQ_MANUAL_CRED_VALIDATION;
    auto target = const_cast<SEC_WCHAR*>(host.c_str());
    std::string token;
    for (;;) {
        SecBuffer in[2] = {{0, SECBUFFER_EMPTY, nullptr}, {0, SECBUFFER_EMPTY, nullptr}};
        SecBufferDesc inDesc{SECBUFFER_VERSION, 2, in};
        if (!token.empty()) {
            in[0].BufferType = SECBUFFER_TOKEN;
            in[0].cbBuffer = DWORD(token.size());
            in[0].pvBuffer = token.data();
        }
        SecBuffer out[1] = {{0, SECBUFFER_TOKEN, nullptr}};
        SecBufferDesc outDesc{SECBUFFER_VERSION, 1, out};
        DWORD attributes = 0;
        const auto status = InitializeSecurityContextW(&credential, haveContext ? &context : nullptr, target, flags, 0,
                                                       0, token.empty() ? nullptr : &inDesc, 0,
                                                       haveContext ? nullptr : &context, &outDesc, &attributes, &expiry);
        if (out[0].cbBuffer && out[0].pvBuffer) {
            raw.send(std::string(static_cast<const char*>(out[0].pvBuffer), out[0].cbBuffer));
            FreeContextBuffer(out[0].pvBuffer);
        }
        if (status == SEC_E_OK) {
            haveContext = true;
            break;
        }
        check(status == SEC_I_CONTINUE_NEEDED, "TLS handshake failed");
        haveContext = true;
        std::string next;
        if (in[1].BufferType == SECBUFFER_EXTRA && in[1].cbBuffer)
            next.assign(static_cast<const char*>(in[1].pvBuffer), in[1].cbBuffer);
        while (next.empty()) {
            char block[16384];
            const int n = raw.recv(block, sizeof(block));
            check(n > 0, "TLS handshake failed");
            next.append(block, size_t(n));
        }
        token.swap(next);
    }
    verify(host);
    check(QueryContextAttributesW(&context, SECPKG_ATTR_STREAM_SIZES, &sizes) == SEC_E_OK, "Cannot size TLS records");
}

void Tls::verify(const std::wstring& host) {
    PCCERT_CONTEXT remote = nullptr;
    check(QueryContextAttributesW(&context, SECPKG_ATTR_REMOTE_CERT_CONTEXT, &remote) == SEC_E_OK && remote,
          "Cannot read the TLS certificate");
    if (const wchar_t* insecure = _wgetenv(L"ODM_TLS_INSECURE"); insecure && *insecure) {
        CertFreeCertificateContext(remote);
        return;
    }
    bool trusted = false;
    CERT_CHAIN_PARA params{};
    params.cbSize = sizeof(params);
    PCCERT_CHAIN_CONTEXT chain = nullptr;
    if (CertGetCertificateChain(nullptr, remote, nullptr, remote->hCertStore, &params, 0, nullptr, &chain) && chain) {
        trusted = chain->TrustStatus.dwErrorStatus == CERT_TRUST_NO_ERROR;
        CertFreeCertificateChain(chain);
    }
    check(trusted, "The TLS certificate is not trusted");
    bool named = false;
    const auto* alternative = CertFindExtension(szOID_SUBJECT_ALT_NAME2, remote->pCertInfo->cExtension,
                                                remote->pCertInfo->rgExtension);
    if (alternative) {
        CERT_ALT_NAME_INFO* info = nullptr;
        DWORD size = 0;
        if (CryptDecodeObjectEx(X509_ASN_ENCODING, X509_ALTERNATE_NAME, alternative->Value.pbData,
                                alternative->Value.cbData, CRYPT_DECODE_ALLOC_FLAG, nullptr, &info, &size)) {
            for (DWORD i = 0; i < info->cAltEntry && !named; ++i) {
                const auto& entry = info->rgAltEntry[i];
                if (entry.dwAltNameChoice == CERT_ALT_NAME_DNS_NAME && entry.pwszDNSName)
                    named = covers(entry.pwszDNSName, host);
            }
            LocalFree(info);
        }
    }
    CertFreeCertificateContext(remote);
    check(named, "The TLS certificate does not match the server name");
}

void Tls::send(const std::string& data) {
    size_t offset = 0;
    while (offset < data.size()) {
        const size_t chunk = std::min<size_t>(data.size() - offset, sizes.cbMaximumMessage);
        std::string record(sizes.cbHeader, '\0');
        record.append(data, offset, chunk);
        record.append(sizes.cbTrailer, '\0');
        // Schannel encrypts through the stream header/data/trailer layout; the
        // single SECBUFFER_STREAM buffer is only used when decrypting.
        SecBuffer buffers[4] = {
            {DWORD(sizes.cbHeader), SECBUFFER_STREAM_HEADER, record.data()},
            {DWORD(chunk), SECBUFFER_DATA, record.data() + sizes.cbHeader},
            {DWORD(sizes.cbTrailer), SECBUFFER_STREAM_TRAILER, record.data() + sizes.cbHeader + chunk},
            {0, SECBUFFER_EMPTY, nullptr},
        };
        SecBufferDesc desc{SECBUFFER_VERSION, 4, buffers};
        const auto status = EncryptMessage(&context, 0, &desc, 0);
        if (status != SEC_E_OK) {
            char message[160];
            sprintf_s(message, "TLS encrypt failed (0x%08lX) hdr=%lu chunk=%zu trailer=%lu",
                      static_cast<unsigned long>(status), (unsigned long)sizes.cbHeader, chunk,
                      (unsigned long)sizes.cbTrailer);
            throw std::runtime_error(message);
        }
        record.resize(size_t(buffers[0].cbBuffer) + buffers[1].cbBuffer + buffers[2].cbBuffer);
        raw.send(record);
        offset += chunk;
    }
}

int Tls::recv(char* data, int size) {
    for (;;) {
        if (!pending.empty()) {
            const auto n = std::min<size_t>(pending.size(), size_t(size));
            std::memcpy(data, pending.data(), n);
            pending.erase(0, n);
            return int(n);
        }
        // Decrypt whatever is already buffered before touching the socket, so a
        // record that arrived together with the end of the stream is not lost.
        if (incoming.empty()) {
            if (closed) return 0;
            char block[16384];
            const int n = raw.recv(block, sizeof(block));
            if (n <= 0) {
                closed = true;
                return 0;
            }
            incoming.append(block, size_t(n));
            continue;
        }
        SecBuffer buffers[4] = {
            {DWORD(incoming.size()), SECBUFFER_DATA, incoming.data()},
            {0, SECBUFFER_EMPTY, nullptr},
            {0, SECBUFFER_EMPTY, nullptr},
            {0, SECBUFFER_EMPTY, nullptr},
        };
        SecBufferDesc desc{SECBUFFER_VERSION, 4, buffers};
        const auto status = DecryptMessage(&context, &desc, 0, nullptr);
        if (status == SEC_E_INCOMPLETE_MESSAGE) {
            char block[16384];
            const int n = raw.recv(block, sizeof(block));
            if (n <= 0) {
                closed = true;
                incoming.clear();
                return 0;
            }
            incoming.append(block, size_t(n));
            continue;
        }
        check(status == SEC_E_OK || status == SEC_I_CONTEXT_EXPIRED || status == SEC_I_RENEGOTIATE, "TLS decrypt failed");
        std::string carry;
        for (const auto& buffer : buffers) {
            if (buffer.BufferType == SECBUFFER_DATA && buffer.cbBuffer)
                pending.append(static_cast<const char*>(buffer.pvBuffer), buffer.cbBuffer);
            else if (buffer.BufferType == SECBUFFER_EXTRA && buffer.cbBuffer)
                carry.assign(static_cast<const char*>(buffer.pvBuffer), buffer.cbBuffer);
        }
        incoming = carry;
        if (status == SEC_I_CONTEXT_EXPIRED || status == SEC_I_RENEGOTIATE) closed = true;
        if (pending.empty() && closed) return 0;
    }
}

void Tls::close() {
    if (haveContext) {
        DeleteSecurityContext(&context);
        haveContext = false;
    }
    if (haveCredential) {
        FreeCredentialsHandle(&credential);
        haveCredential = false;
    }
    closed = true;
    raw.close();
}
}
