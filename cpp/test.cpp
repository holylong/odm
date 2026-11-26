#include "core.h"
#include <future>
#include <iostream>
#include <thread>

int wmain(int argc, wchar_t** argv) {
    if (argc < 3) return 2;
    odm::Progress p;
    try {
        auto work = std::async(std::launch::async, [&] { return odm::download(argv[1], argv[2], 4, p); });
        if (argc == 4) {
            while (p.received < 65536 && work.wait_for(std::chrono::milliseconds(10)) != std::future_status::ready) {}
            p.pause = true;
        }
        return work.get() == (argc == 3) ? 0 : 1;
    } catch (const std::exception& e) { std::cerr << e.what() << '\n'; return 1; }
}
