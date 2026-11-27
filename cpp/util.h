#pragma once
#include <cstdint>
#include <string>

namespace odm {
using Size = std::uint64_t;
void check(bool ok, const char* message);
std::string narrow(const std::wstring& text);
std::wstring widen(const std::string& text);
// Decodes %XX escapes; used for URL paths that reach the wire verbatim.
std::wstring unescapeUrl(const std::wstring& text);
}
