#pragma once
#include <atomic>
#include <cstdint>
#include <filesystem>
#include <string>

namespace odm {
struct Progress {
    std::atomic<std::uint64_t> received{0}, total{0};
    std::atomic<bool> pause{false};
};
// One call per destination at a time. Returns false on pause; throws on failure.
// http/https use WinHTTP with segmented ranges; ftp uses one passive-mode
// connection resumed with REST (the same protocol set IDM speaks).
// Resuming with the same URL and destination reuses validated partial ranges.
bool download(const std::wstring& url, const std::filesystem::path& destination,
              unsigned connections, Progress& progress);
}
