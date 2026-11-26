#include "core.h"
#include <windows.h>
#include <winhttp.h>
#include <algorithm>
#include <array>
#include <cwchar>
#include <fstream>
#include <mutex>
#include <stdexcept>
#include <thread>
#include <vector>

namespace fs = std::filesystem;
namespace odm {
namespace {
using Size = std::uint64_t;
void check(bool ok, const char* message) { if (!ok) throw std::runtime_error(message); }
struct Handle {
    HINTERNET value;
    explicit Handle(HINTERNET h) : value(h) {
        if (!h) throw std::runtime_error("WinHTTP error " + std::to_string(GetLastError()));
    }
    ~Handle() { WinHttpCloseHandle(value); }
    Handle(const Handle&) = delete;
    operator HINTERNET() const { return value; }
};
struct Endpoint {
    std::wstring host, path;
    INTERNET_PORT port;
    bool secure;
    explicit Endpoint(const std::wstring& url) {
        URL_COMPONENTS u{};
        u.dwStructSize = sizeof(u);
        u.dwHostNameLength = u.dwUrlPathLength = u.dwExtraInfoLength = u.dwUserNameLength = u.dwPasswordLength = DWORD(-1);
        check(WinHttpCrackUrl(url.c_str(), 0, 0, &u) &&
              (u.nScheme == INTERNET_SCHEME_HTTP || u.nScheme == INTERNET_SCHEME_HTTPS) &&
              u.dwHostNameLength && !u.dwUserNameLength && !u.dwPasswordLength, "Use an HTTP(S) URL without embedded credentials");
        host.assign(u.lpszHostName, u.dwHostNameLength);
        if (u.dwUrlPathLength) path.assign(u.lpszUrlPath, u.dwUrlPathLength);
        if (path.empty()) path = L"/";
        if (u.dwExtraInfoLength) path.append(u.lpszExtraInfo, u.dwExtraInfoLength);
        if (const auto fragment = path.find(L'#'); fragment != path.npos) path.resize(fragment);
        port = u.nPort;
        secure = u.nScheme == INTERNET_SCHEME_HTTPS;
    }
};
struct Request {
    Handle connection, request;
    DWORD status = 0;
    Request(HINTERNET session, const Endpoint& e, const std::wstring& extra)
        : connection(WinHttpConnect(session, e.host.c_str(), e.port, 0)),
          request(WinHttpOpenRequest(connection, L"GET", e.path.c_str(), nullptr, WINHTTP_NO_REFERER,
                                    WINHTTP_DEFAULT_ACCEPT_TYPES, e.secure ? WINHTTP_FLAG_SECURE : 0)) {
        auto headers = L"Accept-Encoding: identity\r\n" + extra;
        check(WinHttpSendRequest(request, headers.c_str(), DWORD(-1), nullptr, 0, 0, 0) &&
              WinHttpReceiveResponse(request, nullptr), "HTTP connection or response failed");
        DWORD n = sizeof(status);
        check(WinHttpQueryHeaders(request, WINHTTP_QUERY_STATUS_CODE | WINHTTP_QUERY_FLAG_NUMBER,
              nullptr, &status, &n, nullptr), "Missing HTTP status");
    }
    std::wstring header(DWORD field) const {
        DWORD n = 0;
        WinHttpQueryHeaders(request, field, nullptr, nullptr, &n, nullptr);
        if (GetLastError() != ERROR_INSUFFICIENT_BUFFER || n > 65536) return {};
        std::wstring s(n / sizeof(wchar_t), L'\0');
        if (!WinHttpQueryHeaders(request, field, nullptr, s.data(), &n, nullptr)) return {};
        s.resize(n / sizeof(wchar_t));
        return s;
    }
};
std::wstring span(Size first, Size last) { return std::to_wstring(first) + L"-" + std::to_wstring(last); }
void stream(Request& r, const fs::path& file, bool append, Size limit, Progress& p, const std::atomic<bool>& failed) {
    std::ofstream out(file, std::ios::binary | (append ? std::ios::app : std::ios::trunc));
    check(bool(out), "Cannot open partial file");
    std::array<char, 65536> buffer;
    while (!p.pause && !failed) {
        DWORD n = 0;
        check(WinHttpReadData(r.request, buffer.data(), DWORD(buffer.size()), &n), "Download interrupted; retry to resume");
        if (!n) break;
        check(n <= limit, "Server sent excess data");
        out.write(buffer.data(), n);
        check(bool(out), "Cannot write partial file");
        limit -= n;
        p.received += n;
    }
    out.close();
    check(bool(out), "Cannot close partial file");
    check(p.pause || failed || !limit || limit > INT64_MAX, "Response ended before all bytes arrived");
}
}

bool download(const std::wstring& url, const fs::path& destination, unsigned connections, Progress& p) {
    check(!fs::exists(destination), "Destination already exists");
    p.total = p.received = 0;
    Endpoint endpoint(url);
    Handle session(WinHttpOpen(L"ODM/0.1", WINHTTP_ACCESS_TYPE_AUTOMATIC_PROXY, nullptr, nullptr, 0));
    check(WinHttpSetTimeouts(session, 5000, 5000, 5000, 5000), "Cannot set HTTP timeouts");
    auto stage = destination;
    stage += L".odm-work";
    // Hold an exclusive lock across all requests and assembly, including other processes.
    fs::create_directory(stage);
    const auto lockPath = stage / L"lock";
    const auto lock = CreateFileW(lockPath.c_str(), GENERIC_WRITE, 0, nullptr, OPEN_ALWAYS,
                                  FILE_FLAG_DELETE_ON_CLOSE | FILE_ATTRIBUTE_NORMAL, nullptr);
    check(lock != INVALID_HANDLE_VALUE, "Destination is already being downloaded");
    struct Unlock { HANDLE h; ~Unlock() { if (h != INVALID_HANDLE_VALUE) CloseHandle(h); } } guard{lock};
    auto result = stage / L"result";
    Size total = 0;
    std::wstring etag;
    bool ranged = false;
    {
        Request probe(session, endpoint, L"Range: bytes=0-0\r\n");
        const auto range = probe.header(WINHTTP_QUERY_CONTENT_RANGE);
        unsigned long long length = 0;
        ranged = probe.status == 206 && swscanf_s(range.c_str(), L"bytes 0-0/%llu", &length) == 1 &&
                 length > 0 && length <= INT64_MAX && range == L"bytes 0-0/" + std::to_wstring(length);
        if (ranged) total = length;
        etag = probe.header(WINHTTP_QUERY_ETAG);
        if (etag.size() < 2 || etag.front() != L'"' || etag.back() != L'"') etag.clear();
        // Without a strong validator, don't combine independently fetched representations.
        ranged = ranged && !etag.empty();
    }
    if (p.pause) return false;
    std::atomic<bool> failed{false};
    std::vector<fs::path> parts;
    if (!ranged) {
        Request r(session, endpoint, L"");
        if (r.status != 200) throw std::runtime_error("HTTP " + std::to_string(r.status));
        auto length = r.header(WINHTTP_QUERY_CONTENT_LENGTH);
        if (!length.empty()) total = std::stoull(length);
        p.total = total;
        stream(r, result, false, length.empty() ? UINT64_MAX : total, p, failed);
    } else {
        p.total = total;
        const auto count = unsigned(std::min<Size>(std::clamp(connections, 1u, 16u), 1 + (total - 1) / 262144));
        const auto key = url + L"\n" + etag + L"\n" + std::to_wstring(total) + L"\n" + std::to_wstring(count);
        const std::string bytes(reinterpret_cast<const char*>(key.data()), key.size() * sizeof(wchar_t));
        const auto meta = stage / L"identity";
        std::string saved(bytes.size() + 1, '\0');
        { std::ifstream in(meta, std::ios::binary); in.read(saved.data(), saved.size()); saved.resize(size_t(in.gcount())); }
        for (unsigned i = 0; i < count; ++i) parts.push_back(stage / (L"part-" + std::to_wstring(i)));
        if (saved != bytes) {
            for (unsigned i = 0; i < 16; ++i) fs::remove(stage / (L"part-" + std::to_wstring(i)));
            std::ofstream out(meta, std::ios::binary | std::ios::trunc);
            out << bytes;
            out.close();
            check(bool(out), "Cannot save resume metadata");
        }
        std::mutex mutex;
        std::exception_ptr error;
        std::vector<std::thread> workers;
        auto work = [&](unsigned i) {
            try {
                const Size start = (total / count) * i;
                const Size end = i + 1 == count ? total : start + total / count;
                const auto& part = parts[i];
                Size done = fs::exists(part) ? fs::file_size(part) : 0;
                if (done > end - start) { fs::remove(part); done = 0; }
                p.received += done;
                if (done == end - start || p.pause || failed) return;
                auto range = span(start + done, end - 1);
                Request r(session, endpoint, L"Range: bytes=" + range + L"\r\nIf-Match: " + etag + L"\r\n");
                check(r.status == 206 && r.header(WINHTTP_QUERY_ETAG) == etag &&
                      r.header(WINHTTP_QUERY_CONTENT_RANGE) == L"bytes " + range + L"/" + std::to_wstring(total),
                      "Server changed the file or refused the byte range; retry");
                stream(r, part, true, end - start - done, p, failed);
            } catch (...) {
                failed = true;
                std::lock_guard<std::mutex> hold(mutex);
                if (!error) error = std::current_exception();
            }
        };
        try { for (unsigned i = 0; i < count; ++i) workers.emplace_back(work, i); }
        catch (...) { failed = true; for (auto& t : workers) t.join(); throw; }
        for (auto& t : workers) t.join();
        if (p.pause) return false;
        if (error) std::rethrow_exception(error);
        std::ofstream out(result, std::ios::binary | std::ios::trunc);
        check(bool(out), "Cannot assemble output");
        std::array<char, 65536> buffer;
        for (const auto& part : parts) {
            std::ifstream in(part, std::ios::binary);
            check(bool(in), "Cannot read partial file");
            while (!p.pause && in.read(buffer.data(), buffer.size()).gcount()) out.write(buffer.data(), in.gcount());
            if (p.pause) return false;
            check(in.eof() && !in.bad() && bool(out), "Assembly failed");
        }
        out.close();
        check(bool(out) && fs::file_size(result) == total, "Assembled file has incorrect length");
    }
    if (p.pause) return false;
    check(MoveFileW(result.c_str(), destination.c_str()), "Cannot publish output (destination may already exist)");
    std::error_code ignored;
    for (unsigned i = 0; i < 16; ++i) fs::remove(stage / (L"part-" + std::to_wstring(i)), ignored);
    fs::remove(stage / L"identity", ignored);
    CloseHandle(guard.h);
    guard.h = INVALID_HANDLE_VALUE;
    fs::remove(stage, ignored);
    return true;
}
}
