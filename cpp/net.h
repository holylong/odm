#pragma once
// winsock2.h must precede windows.h, otherwise the old winsock.h gets pulled in.
#include <winsock2.h>
#include <windows.h>
#include <schannel.h>
#include <sspi.h>
#include <string>

namespace odm {
// Byte channel used by the FTP and RTMP clients: plain TCP or TLS over TCP.
// Every failure throws std::runtime_error.
struct Channel {
    virtual ~Channel() = default;
    virtual void send(const std::string& data) = 0;
    virtual int recv(char* data, int size) = 0;  // returns 0 at end of stream
    virtual void close() = 0;
};

struct Tcp : Channel {
    Tcp() = default;
    explicit Tcp(SOCKET socket) : value(socket) {}
    ~Tcp() override { close(); }
    Tcp(const Tcp&) = delete;
    Tcp& operator=(const Tcp&) = delete;
    Tcp(Tcp&& other) noexcept : value(other.value) { other.value = INVALID_SOCKET; }
    Tcp& operator=(Tcp&& other) noexcept { std::swap(value, other.value); return *this; }
    static Tcp dial(const std::wstring& host, unsigned short port, int timeoutMs);
    void send(const std::string& data) override;
    int recv(char* data, int size) override;
    void close() override;
    SOCKET value = INVALID_SOCKET;
};

// Schannel client. Certificates are validated against the system roots and the
// host name; set ODM_TLS_INSECURE=1 to accept a self-signed certificate (tests).
struct Tls : Channel {
    Tls(Tcp raw, const std::wstring& host);
    ~Tls() override;
    void send(const std::string& data) override;
    int recv(char* data, int size) override;
    void close() override;

private:
    void handshake(const std::wstring& host);
    void verify(const std::wstring& host);
    Tcp raw;
    CredHandle credential{};
    CtxtHandle context{};
    bool haveCredential = false;
    bool haveContext = false;
    bool closed = false;
    SecPkgContext_StreamSizes sizes{};
    std::string pending;   // decrypted bytes not yet consumed
    std::string incoming;  // raw bytes not yet consumed by DecryptMessage
};
}
