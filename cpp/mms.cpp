#include "mms.h"
#include "util.h"
#include <windows.h>
#include <winhttp.h>
#include <array>
#include <fstream>

namespace fs = std::filesystem;
namespace odm {
namespace {
struct Handle {
    HINTERNET value;
    explicit Handle(HINTERNET h) : value(h) {
        if (!h) throw std::runtime_error("WinHTTP error " + std::to_string(GetLastError()));
    }
    ~Handle() { WinHttpCloseHandle(value); }
    Handle(const Handle&) = delete;
    operator HINTERNET() const { return value; }
};

std::wstring lowerCase(std::wstring text) {
    for (auto& c : text)
        if (c >= L'A' && c <= L'Z') c = wchar_t(c - L'A' + L'a');
    return text;
}

struct Target {
    std::wstring host, path;
    INTERNET_PORT port = 80;
    explicit Target(const std::wstring& url) {
        // Both variants use the MMSH tunnel, which is plain HTTP underneath.
        const auto scheme = lowerCase(url);
        std::wstring http;
        if (scheme.rfind(L"mms://", 0) == 0) http = L"http://" + url.substr(6);
        else http = L"http://" + url.substr(7);
        URL_COMPONENTS u{};
        u.dwStructSize = sizeof(u);
        u.dwHostNameLength = u.dwUrlPathLength = u.dwExtraInfoLength = u.dwUserNameLength = u.dwPasswordLength = DWORD(-1);
        check(WinHttpCrackUrl(http.c_str(), 0, 0, &u) && u.nScheme == INTERNET_SCHEME_HTTP && u.dwHostNameLength,
              "Use an mms:// or mmsh:// URL");
        host.assign(u.lpszHostName, u.dwHostNameLength);
        if (u.dwUrlPathLength) path.assign(u.lpszUrlPath, u.dwUrlPathLength);
        if (path.empty()) path = L"/";
        if (u.dwExtraInfoLength) path.append(u.lpszExtraInfo, u.dwExtraInfoLength);
        port = u.nPort ? u.nPort : 80;
    }
};
}

void downloadMms(const std::wstring& url, const fs::path& result, Progress& p) {
    const Target target(url);
    Handle session(WinHttpOpen(L"NSPlayer/12.0.7600.16385", WINHTTP_ACCESS_TYPE_AUTOMATIC_PROXY, nullptr, nullptr, 0));
    check(WinHttpSetTimeouts(session, 5000, 5000, 10000, 10000), "Cannot set MMS timeouts");
    Handle connection(WinHttpConnect(session, target.host.c_str(), target.port, 0));
    Handle request(WinHttpOpenRequest(connection, L"GET", target.path.c_str(), nullptr, WINHTTP_NO_REFERER,
                                      WINHTTP_DEFAULT_ACCEPT_TYPES, 0));
    // The tunnel headers a Windows Media client sends; servers refuse MMSH without them.
    auto headers = std::wstring(L"Pragma: xClientGUID={3300AD50-2C39-46C0-AE0A-3B7E2C0F0B0E}\r\n"
                                L"Pragma: no-cache\r\nAccept: */*\r\nAccept-Encoding: identity\r\n");
    check(WinHttpSendRequest(request, headers.c_str(), DWORD(-1), nullptr, 0, 0, 0) &&
              WinHttpReceiveResponse(request, nullptr),
          "MMS connection or response failed");
    DWORD status = 0;
    DWORD size = sizeof(status);
    check(WinHttpQueryHeaders(request, WINHTTP_QUERY_STATUS_CODE | WINHTTP_QUERY_FLAG_NUMBER, nullptr, &status, &size, nullptr),
          "Missing MMS status");
    check(status == 200, "The mms server refused the stream");
    size = 0;
    if (WinHttpQueryHeaders(request, WINHTTP_QUERY_CONTENT_LENGTH | WINHTTP_QUERY_FLAG_NUMBER, nullptr, &size, &size, nullptr) &&
        size > 0)
        p.total = size;
    std::ofstream out(result, std::ios::binary | std::ios::trunc);
    check(bool(out), "Cannot open partial file");
    std::array<char, 65536> buffer{};
    while (!p.pause) {
        DWORD n = 0;
        check(WinHttpReadData(request, buffer.data(), DWORD(buffer.size()), &n), "MMS stream interrupted; retry");
        if (!n) break;
        out.write(buffer.data(), n);
        check(bool(out), "Cannot write partial file");
        p.received += n;
    }
    out.close();
    check(bool(out), "Cannot close partial file");
    check(p.pause || p.received > 0, "The mms server sent no media");
}
}
