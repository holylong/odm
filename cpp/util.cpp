#include "util.h"
#include <cstdlib>
#include <stdexcept>
#include <windows.h>

namespace odm {
void check(bool ok, const char* message) {
    if (!ok) throw std::runtime_error(message);
}

std::string narrow(const std::wstring& text) {
    if (text.empty()) return {};
    const int size = WideCharToMultiByte(CP_UTF8, 0, text.data(), int(text.size()), nullptr, 0, nullptr, nullptr);
    std::string out(size_t(size), '\0');
    WideCharToMultiByte(CP_UTF8, 0, text.data(), int(text.size()), out.data(), size, nullptr, nullptr);
    return out;
}

std::wstring widen(const std::string& text) {
    if (text.empty()) return {};
    const int size = MultiByteToWideChar(CP_UTF8, 0, text.data(), int(text.size()), nullptr, 0);
    std::wstring out(size_t(size), L'\0');
    MultiByteToWideChar(CP_UTF8, 0, text.data(), int(text.size()), out.data(), size);
    return out;
}

std::wstring unescapeUrl(const std::wstring& text) {
    std::wstring out;
    out.reserve(text.size());
    for (size_t i = 0; i < text.size(); ++i) {
        if (text[i] == L'%' && i + 2 < text.size()) {
            wchar_t hex[3] = {text[i + 1], text[i + 2], 0};
            wchar_t* end = nullptr;
            const auto code = std::wcstol(hex, &end, 16);
            if (end && *end == 0 && code > 0) { out.push_back(wchar_t(code)); i += 2; continue; }
        }
        out.push_back(text[i]);
    }
    return out;
}
}
