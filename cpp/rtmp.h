#pragma once
#include "core.h"
#include <filesystem>
#include <string>

namespace odm {
// rtmp:// and rtmpt:// play the stream and mux the audio/video/metadata messages
// into an FLV file. rtmps:// (TLS) rides on the same code; rtmpe:// needs the
// Adobe encrypted handshake and is rejected explicitly.
void downloadRtmp(const std::wstring& url, const std::filesystem::path& result, Progress& p);
}
