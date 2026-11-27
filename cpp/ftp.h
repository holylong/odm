#pragma once
#include "core.h"
#include <filesystem>
#include <string>

namespace odm {
// ftp:// (plain) and ftps:// (explicit AUTH TLS + PROT P); single passive-mode
// connection resumed with REST, the same shape IDM's FTPSession implements.
void downloadFtp(const std::wstring& url, const std::filesystem::path& result, Progress& p);
}
