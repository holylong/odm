#include "rtmp.h"
#include "net.h"
#include "util.h"
#include <algorithm>
#include <array>
#include <cctype>
#include <cstring>
#include <fstream>
#include <map>
#include <memory>
#include <random>
#include <utility>
#include <vector>

namespace fs = std::filesystem;
namespace odm {
namespace {
// ─── tiny AMF0 codec ────────────────────────────────────────────────────────
void putNumber(std::string& out, double value) {
    out.push_back(0x00);
    std::uint64_t bits = 0;
    std::memcpy(&bits, &value, 8);
    for (int i = 7; i >= 0; --i) out.push_back(char(bits >> (i * 8)));
}
void putString(std::string& out, const std::string& text) {
    out.push_back(0x02);
    out.push_back(char(text.size() >> 8));
    out.push_back(char(text.size()));
    out += text;
}
void putNull(std::string& out) { out.push_back(0x05); }

std::string encodeString(const std::string& value) {
    std::string out;
    putString(out, value);
    return out;
}

std::string object(std::vector<std::pair<std::string, std::string>> encoded) {
    std::string out;
    for (const auto& [key, value] : encoded) {
        out.push_back(char(key.size() >> 8));
        out.push_back(char(key.size()));
        out += key;
        out += value;
    }
    out.push_back('\0');
    out.push_back('\0');
    out.push_back('\x09');
    return out;
}

struct Amf {
    std::uint8_t kind = 0x05;
    double number = 0;
    std::string text;
    std::vector<std::pair<std::string, Amf>> members;  // object / ECMA array
    std::vector<Amf> items;                            // strict array
};

struct Reader {
    const std::string& data;
    size_t at = 0;
    explicit Reader(const std::string& bytes) : data(bytes) {}
    void need(size_t n) const { check(at + n <= data.size(), "Truncated AMF payload"); }
    std::uint8_t byte() {
        need(1);
        return std::uint8_t(data[at++]);
    }
    std::uint32_t be(size_t n) {
        need(n);
        std::uint32_t value = 0;
        for (size_t i = 0; i < n; ++i) value = (value << 8) | std::uint8_t(data[at++]);
        return value;
    }
    std::string raw(size_t n) {
        need(n);
        std::string out = data.substr(at, n);
        at += n;
        return out;
    }
    double number() {
        need(8);
        std::uint64_t bits = 0;
        for (int i = 0; i < 8; ++i) bits = (bits << 8) | std::uint8_t(data[at++]);
        double value = 0;
        std::memcpy(&value, &bits, 8);
        return value;
    }
    void members(Amf& into) {
        for (;;) {
            need(2);
            const auto nameSize = be(2);
            if (nameSize == 0 && at < data.size() && data[at] == '\x09') { at += 1; break; }
            auto name = raw(nameSize);
            into.members.emplace_back(std::move(name), read());
        }
    }
    Amf read() {
        Amf value;
        value.kind = byte();
        switch (value.kind) {
        case 0x00: value.number = number(); break;
        case 0x01: value.number = byte() != 0; value.kind = 0x01; break;
        case 0x02: value.text = raw(be(2)); break;
        case 0x03: members(value); break;
        case 0x05:
        case 0x06: break;
        case 0x08:  // ECMA array: the count is only a hint, entries run to the marker
            be(4);
            members(value);
            break;
        case 0x0A: {
            const auto count = be(4);
            for (std::uint32_t i = 0; i < count; ++i) value.items.push_back(read());
            break;
        }
        case 0x0C: value.text = raw(be(4)); break;
        default: throw std::runtime_error("Unsupported AMF0 value in the stream reply");
        }
        return value;
    }
};

// ─── RTMP chunk stream ──────────────────────────────────────────────────────
struct Message {
    std::uint8_t type = 0;
    std::uint32_t stream = 0;
    std::uint32_t timestamp = 0;
    std::string payload;
};

struct ChunkState {
    std::uint32_t timestamp = 0, length = 0, stream = 0;
    std::uint8_t type = 0;
    std::string payload;
};

struct Url {
    std::wstring host;
    std::string app, stream, tcUrl;
    unsigned short port = 1935;
    enum Mode { Plain, Tls, Tunnel } mode = Plain;
    explicit Url(const std::wstring& url) {
        const auto split = url.find(L"://");
        check(split != std::wstring::npos, "Use an rtmp:// URL");
        auto scheme = url.substr(0, split);
        for (auto& c : scheme)
            if (c >= L'A' && c <= L'Z') c = wchar_t(c - L'A' + L'a');
        check(scheme != L"rtmpe", "Encrypted RTMP (rtmpe://) needs the Adobe secure handshake, which is not implemented yet");
        const bool tunnel = scheme == L"rtmpt";
        const bool tls = scheme == L"rtmps";
        mode = tls ? Tls : (tunnel ? Tunnel : Plain);
        if (tunnel) port = 80;
        else if (tls) port = 443;
        auto rest = url.substr(split + 3);
        const auto slash = rest.find(L'/');
        auto authority = slash == std::wstring::npos ? rest : rest.substr(0, slash);
        auto path = slash == std::wstring::npos ? std::wstring() : rest.substr(slash + 1);
        const auto colon = authority.find(L':');
        if (colon != std::wstring::npos) {
            port = (unsigned short)std::wcstol(authority.substr(colon + 1).c_str(), nullptr, 10);
            authority = authority.substr(0, colon);
        }
        check(!authority.empty() && !path.empty(), "The rtmp URL needs a host and an app/stream path");
        host = authority;
        const auto cut = path.find(L'/');
        app = narrow(cut == std::wstring::npos ? path : path.substr(0, cut));
        stream = narrow(cut == std::wstring::npos ? path : path.substr(cut + 1));
        check(!app.empty() && !stream.empty(), "The rtmp URL needs an app and a stream name");
        const auto prefix = tunnel ? L"rtmpt://" : (tls ? L"rtmps://" : L"rtmp://");
        tcUrl = narrow(prefix + host + L":" + std::to_wstring(port) + L"/" + widen(app));
    }
};

// RTMPT: the chunk stream rides on HTTP POSTs against /open, /send and /idle.
class Tunnel : public Channel {
public:
    Tunnel(const Url& url, int timeoutMs) : host(url.host), port(url.port), timeout(timeoutMs) {}

    void send(const std::string& data) override { outgoing += data; }

    int recv(char* data, int size) override {
        if (incoming.empty() && !refill()) return 0;
        const auto n = std::min<size_t>(incoming.size(), size_t(size));
        std::memcpy(data, incoming.data(), n);
        incoming.erase(0, n);
        return int(n);
    }

    void close() override {
        if (wire.value != INVALID_SOCKET) wire.close();
        closed = true;
    }

private:
    bool refill() {
        if (closed) return false;
        if (wire.value == INVALID_SOCKET) {
            wire = Tcp::dial(host, port, timeout);
            // /open carries no payload: keep whatever is queued (the RTMP
            // handshake) for the first /send so the peer reads it as stream data.
            auto queued = std::move(outgoing);
            outgoing.clear();
            incoming = request("open");
            outgoing = std::move(queued);
            return true;
        }
        incoming = request(outgoing.empty() ? "idle" : "send");
        return true;
    }

    std::string request(const std::string& verb) {
        const auto body = outgoing;
        outgoing.clear();
        const auto path = "/" + verb + "/" + std::to_string(++sequence);
        std::string head = "POST " + path + " HTTP/1.1\r\nHost: " + narrow(host) + ":" + std::to_string(port) +
                           "\r\nContent-Type: application/x-fcs\r\nContent-Length: " + std::to_string(body.size()) +
                           "\r\nConnection: keep-alive\r\n\r\n";
        wire.send(head + body);
        std::string header;
        while (header.find("\r\n\r\n") == std::string::npos) {
            char block[4096];
            const int n = wire.recv(block, sizeof(block));
            check(n > 0, "RTMPT tunnel closed");
            header.append(block, size_t(n));
            check(header.size() < 65536, "RTMPT response headers too large");
        }
        const auto split = header.find("\r\n\r\n");
        const auto meta = header.substr(0, split);
        std::string rest = header.substr(split + 4);
        size_t length = 0;
        bool chunked = false;
        for (auto line = meta.find("\r\n"); line != std::string::npos;) {
            const auto next = meta.find("\r\n", line + 2);
            auto text = meta.substr(line + 2, next == std::string::npos ? std::string::npos : next - line - 2);
            std::transform(text.begin(), text.end(), text.begin(),
                           [](char c) { return char(std::tolower((unsigned char)c)); });
            if (text.rfind("content-length:", 0) == 0) length = std::stoul(text.substr(15));
            if (text.rfind("transfer-encoding:", 0) == 0 && text.find("chunked") != std::string::npos) chunked = true;
            line = next;
        }
        if (chunked) {
            std::string out;
            for (;;) {
                while (rest.find("\r\n") == std::string::npos) {
                    char block[4096];
                    const int n = wire.recv(block, sizeof(block));
                    check(n > 0, "RTMPT tunnel closed");
                    rest.append(block, size_t(n));
                }
                const auto lineEnd = rest.find("\r\n");
                const auto chunkSize = std::stoul(rest.substr(0, lineEnd), nullptr, 16);
                if (chunkSize == 0) break;
                while (rest.size() < lineEnd + 2 + chunkSize + 2) {
                    char block[4096];
                    const int n = wire.recv(block, sizeof(block));
                    check(n > 0, "RTMPT tunnel closed");
                    rest.append(block, size_t(n));
                }
                out += rest.substr(lineEnd + 2, chunkSize);
                rest.erase(0, lineEnd + 2 + chunkSize + 2);
            }
            return out;
        }
        while (rest.size() < length) {
            char block[4096];
            const int n = wire.recv(block, sizeof(block));
            check(n > 0, "RTMPT tunnel closed");
            rest.append(block, size_t(n));
        }
        return rest.substr(0, length);
    }

    Tcp wire;
    std::wstring host;
    unsigned short port;
    int timeout;
    std::string outgoing, incoming;
    unsigned sequence = 0;
    bool closed = false;
};

class Rtmp {
public:
    Rtmp(std::unique_ptr<Channel> channel, Progress& progress) : link(std::move(channel)), p(progress) {}

    void play(const Url& url, const fs::path& file) {
        handshake();
        // Announce a larger chunk size before the first command: "connect" does
        // not fit into the default 128-byte chunk and would need continuation
        // chunks that some minimal servers refuse to parse.
        std::string limit;
        outbound = 4096;
        limit.push_back(char(outbound >> 24 & 0xFF));
        limit.push_back(char(outbound >> 16 & 0xFF));
        limit.push_back(char(outbound >> 8 & 0xFF));
        limit.push_back(char(outbound & 0xFF));
        sendMessage(2, 1, 0, limit, 0);
        std::string connect;
        putString(connect, "connect");
        putNumber(connect, 1);
        connect += object({{"app", encodeString(url.app)},
                           {"type", encodeString("nonprivate")},
                           {"flashVer", encodeString("FMLE/3.0 (compatible; FMSc/1.0)")},
                           {"tcUrl", encodeString(url.tcUrl)}});
        sendMessage(3, 20, 0, connect, 0);

        std::uint32_t streamId = 0;
        bool connected = false;
        while (streamId == 0) {
            const auto message = next();
            if (message.type == 1) {
                consumeChunkSize(message.payload);
                continue;
            }
            if (message.type != 20) continue;
            Reader reader(message.payload);
            const auto name = reader.read();
            if (name.kind != 0x02) continue;
            if (name.text == "_error") throw std::runtime_error("RTMP server rejected the request");
            if (name.text != "_result") continue;
            const auto transaction = reader.read();
            if (!connected) {
                connected = true;
                std::string create;
                putString(create, "createStream");
                putNumber(create, 2);
                putNull(create);
                sendMessage(3, 20, 0, create, 0);
                continue;
            }
            while (reader.at < reader.data.size()) {
                const auto value = reader.read();
                if (value.kind == 0x00 && value.number > 0) {
                    streamId = std::uint32_t(value.number);
                    break;
                }
            }
        }
        playStream(streamId, url, file);
    }

private:
    void consumeChunkSize(const std::string& payload) {
        if (payload.size() < 4) return;
        inbound = (std::uint32_t(std::uint8_t(payload[0])) << 24 | std::uint32_t(std::uint8_t(payload[1])) << 16 |
                   std::uint32_t(std::uint8_t(payload[2])) << 8 | std::uint32_t(std::uint8_t(payload[3]))) &
                  0x7FFFFFFF;
    }

    void handshake() {
        std::string c0c1(1537, '\0');
        c0c1[0] = char(3);
        std::mt19937 rng(std::random_device{}());
        for (size_t i = 8; i < c0c1.size(); ++i) c0c1[i] = char(rng() & 0xFF);
        link->send(c0c1);
        const auto s0 = takeBytes(1);
        check(s0[0] == 3, "Unsupported RTMP version");
        const auto s1 = takeBytes(1536);
        takeBytes(1536);   // S2, echoed back unverified
        link->send(s1);    // C2 echoes S1, which is what the simple handshake expects
    }

    std::string takeBytes(size_t n) {
        std::string out;
        while (out.size() < n) {
            if (!buffer.empty()) {
                const auto take = std::min<size_t>(buffer.size(), n - out.size());
                out += buffer.substr(0, take);
                buffer.erase(0, take);
                continue;
            }
            std::array<char, 16384> block{};
            const auto want = int(std::min<size_t>(block.size(), n - out.size()));
            const int got = link->recv(block.data(), want);
            check(got > 0, "RTMP connection closed");
            out.append(block.data(), size_t(got));
        }
        return out;
    }

    std::uint8_t takeByte() {
        const auto bytes = takeBytes(1);
        return std::uint8_t(bytes[0]);
    }

    std::uint32_t be(size_t n) {
        std::uint32_t value = 0;
        for (size_t i = 0; i < n; ++i) value = (value << 8) | takeByte();
        return value;
    }
    std::uint32_t le(size_t n) {
        std::uint32_t value = 0;
        for (size_t i = 0; i < n; ++i) value |= std::uint32_t(takeByte()) << (8 * i);
        return value;
    }

    void sendMessage(std::uint8_t csid, std::uint8_t type, std::uint32_t stream, const std::string& payload,
                     std::uint32_t timestamp) {
        std::string out;
        out.push_back(char(csid & 0x3F));
        out.push_back(char((timestamp >> 16) & 0xFF));
        out.push_back(char((timestamp >> 8) & 0xFF));
        out.push_back(char(timestamp & 0xFF));
        out.push_back(char((payload.size() >> 16) & 0xFF));
        out.push_back(char((payload.size() >> 8) & 0xFF));
        out.push_back(char(payload.size() & 0xFF));
        out.push_back(char(type));
        for (int i = 0; i < 4; ++i) out.push_back(char((stream >> (8 * i)) & 0xFF));
        size_t offset = 0;
        while (offset < payload.size()) {
            const auto chunk = std::min<size_t>(outbound, payload.size() - offset);
            out.append(payload, offset, chunk);
            offset += chunk;
            if (offset < payload.size()) out.push_back(char(0xC0 | (csid & 0x3F)));
        }
        link->send(out);
    }

    void playStream(std::uint32_t streamId, const Url& url, const fs::path& file) {
        std::string play;
        putString(play, "play");
        putNumber(play, 3);
        putNull(play);
        putString(play, url.stream);
        putNumber(play, -2);
        sendMessage(8, 20, streamId, play, 0);

        std::ofstream out(file, std::ios::binary | std::ios::trunc);
        check(bool(out), "Cannot open partial file");
        const char header[13] = {'F', 'L', 'V', 1, 5, 0, 0, 0, 9, 0, 0, 0, 0};
        out.write(header, sizeof(header));
        check(bool(out), "Cannot write FLV header");
        while (!p.pause) {
            Message message;
            try {
                message = next();
            } catch (const std::runtime_error&) {
                break;  // the publisher stopped: everything captured so far is the file
            }
            if (message.type == 1) {
                consumeChunkSize(message.payload);
                continue;
            }
            if (message.type == 20) {
                if (stopStatus(message.payload)) break;
                continue;
            }
            if (message.type != 8 && message.type != 9 && message.type != 18) continue;
            const auto size = std::uint32_t(message.payload.size());
            const char tag[11] = {char(message.type),
                                  char((size >> 16) & 0xFF), char((size >> 8) & 0xFF), char(size & 0xFF),
                                  char((message.timestamp >> 16) & 0xFF), char((message.timestamp >> 8) & 0xFF),
                                  char(message.timestamp & 0xFF), char((message.timestamp >> 24) & 0xFF),
                                  0, 0, 0};
            out.write(tag, sizeof(tag));
            out.write(message.payload.data(), std::streamsize(message.payload.size()));
            const std::uint32_t total = size + 11;
            const char previous[4] = {char((total >> 24) & 0xFF), char((total >> 16) & 0xFF), char((total >> 8) & 0xFF),
                                      char(total & 0xFF)};
            out.write(previous, sizeof(previous));
            check(bool(out), "Cannot write FLV tag");
            p.received += size;
        }
        out.close();
        check(bool(out), "Cannot close partial file");
    }

    bool stopStatus(const std::string& payload) {
        Reader reader(payload);
        try {
            const auto name = reader.read();
            if (name.kind != 0x02) return false;
            while (reader.at < reader.data.size()) {
                const auto value = reader.read();
                if (value.kind != 0x03) continue;
                for (const auto& [key, member] : value.members) {
                    if (key != "code" || member.kind != 0x02) continue;
                    const auto& code = member.text;
                    if (code == "NetStream.Play.Stop" || code == "NetStream.Play.UnpublishNotify" ||
                        code == "NetStream.Play.Complete")
                        return true;
                }
            }
        } catch (const std::runtime_error&) {
            return false;
        }
        return false;
    }

    Message next() {
        for (;;) {
            const auto basic = takeByte();
            const auto fmt = std::uint8_t(basic >> 6);
            std::uint32_t csid = basic & 0x3F;
            if (csid == 0) csid = 64 + takeByte();
            else if (csid == 1) csid = 64 + (std::uint32_t(takeByte()) | (std::uint32_t(takeByte()) << 8));
            auto& state = chunks[csid];
            if (fmt == 0) {
                state.timestamp = be(3);
                state.length = be(3);
                state.type = takeByte();
                state.stream = le(4);
                state.payload.clear();
                if (state.timestamp == 0xFFFFFF) state.timestamp = le(4);
            } else if (fmt == 1) {
                const auto delta = be(3);
                state.length = be(3);
                state.type = takeByte();
                state.payload.clear();
                state.timestamp += delta == 0xFFFFFF ? le(4) : delta;
            } else if (fmt == 2) {
                const auto delta = be(3);
                state.payload.clear();
                state.timestamp += delta == 0xFFFFFF ? le(4) : delta;
            }
            const auto want = std::min<std::uint32_t>(inbound, state.length - std::uint32_t(state.payload.size()));
            state.payload += takeBytes(want);
            if (state.payload.size() != state.length) continue;
            Message message{state.type, state.stream, state.timestamp, state.payload};
            state.payload.clear();
            return message;
        }
    }

    std::unique_ptr<Channel> link;
    Progress& p;
    std::string buffer;
    std::map<std::uint32_t, ChunkState> chunks;
    std::uint32_t inbound = 128;
    std::uint32_t outbound = 128;
};
}

void downloadRtmp(const std::wstring& url, const fs::path& result, Progress& p) {
    const Url target(url);
    std::unique_ptr<Channel> link;
    if (target.mode == Url::Tunnel) {
        link = std::make_unique<Tunnel>(target, 5000);
    } else {
        Tcp socket = Tcp::dial(target.host, target.port, 5000);
        if (target.mode == Url::Tls) link = std::make_unique<Tls>(std::move(socket), target.host);
        else link = std::make_unique<Tcp>(std::move(socket));
    }
    Rtmp client(std::move(link), p);
    client.play(target, result);
    check(p.pause || p.received > 0, "The rtmp stream carried no audio or video");
}
}
