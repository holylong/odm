#pragma once
#include "core.h"
#include <filesystem>
#include <string>

namespace odm {
// mms:// and mmsh:// stream over the MMSH tunnel: an HTTP GET carrying the
// Windows Media client GUID pragma, saved verbatim as the ASF payload.
void downloadMms(const std::wstring& url, const std::filesystem::path& result, Progress& p);
}
