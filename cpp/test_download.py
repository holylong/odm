import http.server
import pathlib
import random
import subprocess
import sys
import tempfile
import threading
import time

exe = sys.argv[1]
payload = random.Random(19).randbytes(2 * 1024 * 1024)
ranges = []


class Server(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        start, end = 0, len(payload) - 1
        byte_range = self.headers.get('Range') if self.path == '/range' else None
        if byte_range:
            start, end = map(int, byte_range.removeprefix('bytes=').split('-'))
            ranges.append(start)
        self.send_response(206 if byte_range else 200)
        self.send_header('ETag', '"fixture"')
        if byte_range:
            self.send_header('Content-Range', f'bytes {start}-{end}/{len(payload)}')
        self.send_header('Content-Length', str(end - start + 1))
        self.end_headers()
        try:
            for i in range(start, end + 1, 8192):
                self.wfile.write(payload[i:min(i + 8192, end + 1)])
                if byte_range:
                    time.sleep(0.008)
        except (ConnectionError, OSError):
            pass


with http.server.ThreadingHTTPServer(('127.0.0.1', 0), Server) as server, tempfile.TemporaryDirectory() as tmp:
    threading.Thread(target=server.serve_forever, daemon=True).start()
    url = f'http://127.0.0.1:{server.server_port}'
    dest = pathlib.Path(tmp) / 'test.bin'
    subprocess.run([exe, url + '/range', dest, 'pause'], check=True, timeout=20)
    assert not dest.exists()
    ranges.clear()
    subprocess.run([exe, url + '/range', dest], check=True, timeout=20)
    assert any(start % (len(payload) // 4) for start in ranges), ranges
    assert dest.read_bytes() == payload
    assert not pathlib.Path(str(dest) + '.odm-work').exists()
    plain = pathlib.Path(tmp) / 'plain.bin'
    subprocess.run([exe, url + '/plain', plain], check=True, timeout=10)
    assert plain.read_bytes() == payload
    server.shutdown()
