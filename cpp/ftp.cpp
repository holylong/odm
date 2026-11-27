#include "ftp.h"
#include "net.h"
#include "util.h"
#include <winhttp.h>
#include <array>
#include <cstdlib>
#include <fstream>
#include <memory>
#include <utility>

namespace fs = std::filesystem;
namespace odm {
namespace {
struct FtpUrl {
    std::wstring host, path, user, pass;
    INTERNET_PORT port = 21;
    bool secure = false;
    explicit FtpUrl(const std::wstring& url) {
        secure = url.size() >= 7 && _wcsnicmp(url.c_str(), L"ftps://", 7) == 0;
        // WinHTTP has no "ftps" scheme; crack the equivalent ftp URL and keep the flag.
        const auto text = secure ? L"ftp://" + url.substr(7) : url;
        URL_COMPONENTS u{};
        u.dwStructSize = sizeof(u);
        u.dwHostNameLength = u.dwUrlPathLength = u.dwExtraInfoLength = u.dwUserNameLength = u.dwPasswordLength = DWORD(-1);
        check(WinHttpCrackUrl(text.c_str(), 0, 0, &u) && u.nScheme == INTERNET_SCHEME_FTP && u.dwHostNameLength,
              "Use an ftp:// or ftps:// URL");
        host.assign(u.lpszHostName, u.dwHostNameLength);
        std::wstring rawPath;
        if (u.dwUrlPathLength) rawPath.assign(u.lpszUrlPath, u.dwUrlPathLength);
        if (u.dwExtraInfoLength) rawPath.append(u.lpszExtraInfo, u.dwExtraInfoLength);
        path = unescapeUrl(rawPath);
        check(path.size() > 1, "The ftp URL has no file path");
        if (u.dwUserNameLength) user.assign(u.lpszUserName, u.dwUserNameLength);
        if (u.dwPasswordLength) pass.assign(u.lpszPassword, u.dwPasswordLength);
        port = u.nPort ? u.nPort : 21;
    }
};

struct Reply {
    int code = 0;
    std::string text;
};

class FtpClient {
public:
    FtpClient(const FtpUrl& target, int timeoutMs) : host(target.host), secure(target.secure), timeout(timeoutMs) {
        wire = Tcp::dial(target.host, target.port, timeoutMs);
        const auto welcome = readReply();
        check(welcome.code / 100 == 2, "Ftp server doesn't allow connections");
        const auto name = target.user.empty() ? std::wstring(L"anonymous") : target.user;
        const auto secret = target.user.empty() ? std::wstring(L"anonymous@") : target.pass;
        auto reply = cmd(L"USER " + name);
        check(reply.code / 100 == 2 || reply.code / 100 == 3, "Cannot login to ftp server");
        if (reply.code / 100 == 3) {
            reply = cmd(L"PASS " + secret);
            check(reply.code / 100 == 2, "FTP password rejected");
        }
        if (secure) {
            reply = cmd(L"AUTH TLS");
            check(reply.code == 234, "This ftp server doesn't support FTPS (AUTH TLS)");
            tls = std::make_unique<Tls>(std::move(wire), host);
            cmd(L"PBSZ 0");
            reply = cmd(L"PROT P");
            check(reply.code / 100 == 2, "This ftp server refused PROT P");
        }
        cmd(L"TYPE I");
    }
    // The destructor may run while unwinding (see the pause path), so a closed
    // control connection must never raise from here.
    ~FtpClient() {
        try { cmd(L"QUIT"); } catch (...) {}
    }

    std::int64_t size(const std::wstring& path) {
        const auto reply = cmd(L"SIZE " + path);
        if (reply.code != 213) return -1;
        std::string text = reply.text;
        while (!text.empty() && (text.back() == '\n' || text.back() == '\r' || text.back() == ' ')) text.pop_back();
        const auto space = text.find_last_of(" \t");
        const auto digits = space == std::string::npos ? text : text.substr(space + 1);
        if (digits.empty() || digits.find_first_not_of("0123456789") != std::string::npos) return -1;
        return std::strtoll(digits.c_str(), nullptr, 10);
    }

    // Streams the file into `file` from `offset`. The data connection is only
    // wrapped in TLS after RETR: FTPS servers accept and handshake on it then.
    void retr(const std::wstring& path, Size offset, const fs::path& file, Progress& p) {
        const auto address = passive();
        Tcp raw = Tcp::dial(address.first, address.second, timeout);
        if (offset) {
            const auto reply = cmd(L"REST " + std::to_wstring(offset));
            check(reply.code / 100 == 3, "This ftp server doesn't support download resume");
        }
        const auto start = cmd(L"RETR " + path);
        check(start.code / 100 == 1, "Cannot find this file on ftp server");
        std::unique_ptr<Channel> data;
        if (secure) data = std::make_unique<Tls>(std::move(raw), host);
        else data = std::make_unique<Tcp>(std::move(raw));
        std::ofstream out(file, std::ios::binary | std::ios::app);
        check(bool(out), "Cannot open partial file");
        std::array<char, 65536> block{};
        while (!p.pause) {
            const int n = data->recv(block.data(), int(block.size()));
            if (n == 0) break;
            out.write(block.data(), n);
            check(bool(out), "Cannot write partial file");
            p.received += Size(n);
        }
        out.close();
        check(bool(out), "Cannot close partial file");
    }

private:
    Channel& link() { return tls ? static_cast<Channel&>(*tls) : static_cast<Channel&>(wire); }

    Reply cmd(const std::wstring& line) {
        if (wire.value == INVALID_SOCKET && !tls) return {};
        const auto bytes = narrow(line + L"\r\n");
        link().send(bytes);
        return readReply();
    }

    Reply readReply() {
        Reply reply;
        std::string text;
        for (;;) {
            auto newline = buffer.find('\n');
            while (newline == std::string::npos) {
                std::array<char, 2048> chunk{};
                const int n = link().recv(chunk.data(), int(chunk.size()));
                check(n > 0, "Error occurred while receiving ftp server reply");
                buffer.append(chunk.data(), size_t(n));
                newline = buffer.find('\n');
            }
            std::string line = buffer.substr(0, newline + 1);
            buffer.erase(0, newline + 1);
            text += line;
            while (!line.empty() && (line.back() == '\n' || line.back() == '\r')) line.pop_back();
            check(line.size() >= 4, "Cannot find the end of string in ftp server reply");
            const int code = std::atoi(line.substr(0, 3).c_str());
            check(code >= 100 && code <= 599, "Invalid reply format");
            if (line[3] == '-') continue;  // continuation line, e.g. "211-Features:"
            reply.code = code;
            reply.text = text;
            return reply;
        }
    }

    std::pair<std::wstring, unsigned short> passive() {
        auto reply = cmd(L"EPSV");
        if (reply.code == 229) {
            const auto open = reply.text.find("(|||");
            if (open != std::string::npos) {
                const auto rest = reply.text.substr(open + 4);
                const auto bar = rest.find('|');
                if (bar != std::string::npos && bar > 0 && bar <= 5) {
                    const auto port = std::atoi(rest.substr(0, bar).c_str());
                    if (port > 0) return {host, (unsigned short)port};
                }
            }
        }
        reply = cmd(L"PASV");
        check(reply.code == 227, "ftp server doesn't support PASV");
        const auto open = reply.text.find('(');
        const auto close = open == std::string::npos ? std::string::npos : reply.text.find(')', open);
        check(open != std::string::npos && close != std::string::npos && close > open, "Cannot parse the PASV reply");
        std::vector<int> fields;
        const std::string numbers = reply.text.substr(open + 1, close - open - 1);
        size_t start = 0;
        for (;;) {
            const auto comma = numbers.find(',', start);
            fields.push_back(std::atoi(numbers.substr(start, comma == std::string::npos ? std::string::npos : comma - start).c_str()));
            if (comma == std::string::npos) break;
            start = comma + 1;
        }
        check(fields.size() == 6, "Cannot parse the PASV reply");
        std::wstring server = std::to_wstring(fields[0]) + L"." + std::to_wstring(fields[1]) + L"." +
                              std::to_wstring(fields[2]) + L"." + std::to_wstring(fields[3]);
        // Many servers advertise a NAT address; prefer the control peer when that happens.
        if (fields[0] == 0 || (fields[0] == 127 && host.rfind(L"127.", 0) != 0)) server = host;
        const auto port = fields[4] * 256 + fields[5];
        check(port > 0 && port < 65536, "Cannot parse the PASV reply");
        return {server, (unsigned short)port};
    }

    Tcp wire;
    std::unique_ptr<Tls> tls;
    std::wstring host;
    bool secure;
    int timeout;
    std::string buffer;
};
}

void downloadFtp(const std::wstring& url, const fs::path& result, Progress& p) {
    const FtpUrl target(url);
    FtpClient client(target, 5000);
    const auto remote = client.size(target.path);
    const Size known = remote > 0 ? Size(remote) : 0;
    Size offset = 0;
    if (known) {
        offset = fs::exists(result) ? Size(fs::file_size(result)) : 0;
        if (offset > known) { fs::remove(result); offset = 0; }
        p.total = known;
        p.received = offset;
    } else {
        // Without SIZE there is no safe resume point, so restart from the beginning.
        fs::remove(result);
        p.total = 0;
        p.received = 0;
    }
    if (known && offset == known) return;
    client.retr(target.path, offset, result, p);
    if (p.pause) return;
    if (known) {
        const Size produced = fs::exists(result) ? Size(fs::file_size(result)) : 0;
        if (produced != known) {
            char message[128];
            sprintf_s(message, "The file size on the ftp server changed (expected %llu, got %llu); retry from the beginning",
                      static_cast<unsigned long long>(known), static_cast<unsigned long long>(produced));
            check(false, message);
        }
    }
}
}
