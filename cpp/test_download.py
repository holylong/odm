import http.server
import pathlib
import random
import socket
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


class FtpServer:
    """Minimal passive-mode FTP server: USER/PASS, TYPE, SIZE, EPSV, PASV, REST, RETR."""

    def __init__(self, files):
        self.files = files
        self.rests = []
        self.sock = socket.socket()
        self.sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self.sock.bind(('127.0.0.1', 0))
        self.sock.listen(8)
        self.port = self.sock.getsockname()[1]
        threading.Thread(target=self.accept_loop, daemon=True).start()

    def accept_loop(self):
        while True:
            try:
                conn, _ = self.sock.accept()
            except OSError:
                return
            threading.Thread(target=self.serve, args=(conn,), daemon=True).start()

    def serve(self, conn):
        try:
            self.handle(conn)
        except (OSError, ValueError):
            pass
        finally:
            conn.close()

    def handle(self, conn):
        stream = conn.makefile('rwb')
        listening = None
        rest = 0

        def reply(line):
            stream.write((line + '\r\n').encode())
            stream.flush()

        def listener():
            sock = socket.socket()
            sock.bind(('127.0.0.1', 0))
            sock.listen(1)
            return sock

        reply('220 odm test ftp ready')
        while True:
            line = stream.readline()
            if not line:
                return
            verb, _, arg = line.decode('latin-1').strip().partition(' ')
            verb = verb.upper()
            if verb == 'USER':
                reply('331 password required')
            elif verb == 'PASS':
                reply('230 logged in')
            elif verb == 'TYPE':
                reply('200 switched to binary')
            elif verb == 'SIZE':
                blob = self.files.get(arg.lstrip('/'))
                reply(f'213 {len(blob)}' if blob is not None else '550 no such file')
            elif verb == 'REST':
                rest = int(arg)
                self.rests.append(rest)
                reply('350 restarting at requested offset')
            elif verb == 'EPSV':
                listening = listener()
                reply(f'229 Entering Extended Passive Mode (|||{listening.getsockname()[1]}|)')
            elif verb == 'PASV':
                listening = listener()
                port = listening.getsockname()[1]
                reply(f'227 Entering Passive Mode (127,0,0,1,{port // 256},{port % 256})')
            elif verb == 'RETR':
                blob = self.files.get(arg.lstrip('/'))
                if blob is None:
                    reply('550 no such file')
                    continue
                reply('150 opening data connection')
                data, _ = listening.accept()
                try:
                    for i in range(rest, len(blob), 8192):
                        data.sendall(blob[i:min(i + 8192, len(blob))])
                        time.sleep(0.004)
                finally:
                    data.close()
                    listening.close()
                    listening = None
                rest = 0
                reply('226 transfer complete')
            elif verb == 'QUIT':
                reply('221 bye')
                return
            else:
                reply('502 command not implemented')


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

    # ftp:// — one passive-mode connection, resumed with REST like IDM's FTP session.
    ftp = FtpServer({'demo.bin': payload})
    ftp_url = f'ftp://127.0.0.1:{ftp.port}/demo.bin'
    ftp_dest = pathlib.Path(tmp) / 'ftp.bin'
    subprocess.run([exe, ftp_url, ftp_dest, 'pause'], check=True, timeout=20)
    assert not ftp_dest.exists()
    partial = pathlib.Path(str(ftp_dest) + '.odm-work') / 'result'
    assert partial.exists() and partial.stat().st_size > 0
    resumed_from = partial.stat().st_size
    ftp.rests.clear()
    subprocess.run([exe, ftp_url, ftp_dest], check=True, timeout=60)
    assert ftp.rests == [resumed_from], ftp.rests
    assert ftp_dest.read_bytes() == payload
    assert not pathlib.Path(str(ftp_dest) + '.odm-work').exists()
    fresh = pathlib.Path(tmp) / 'ftp-fresh.bin'
    subprocess.run([exe, ftp_url, fresh], check=True, timeout=60)
    assert fresh.read_bytes() == payload
    ftp.sock.close()
