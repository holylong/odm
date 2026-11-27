"""Protocol coverage for ftps://, mms://|mmsh:// and rtmp://|rtmpt://.

Runs the real odm_core_test binary against local servers: a TLS-wrapped passive
FTP server (AUTH TLS + PROT P), an MMSH tunnel, and a fake RTMP publisher that
speaks the handshake, AMF0 connect/createStream/play and emits audio/video/
metadata messages which must land in the FLV untouched.
"""
import http.server
import os
import pathlib
import random
import shutil
import socket
import ssl
import struct
import subprocess
import sys
import tempfile
import threading
import time

exe = sys.argv[1]
payload = random.Random(23).randbytes(512 * 1024)
asf_payload = b'\x30\x26\xb2\x75\x8e\x66\xcf\x11' + random.Random(41).randbytes(96 * 1024)

failures = []


def expect(condition, message):
    if not condition:
        failures.append(message)
        print('FAIL', message)


# ─── ftps:// ─────────────────────────────────────────────────────────────────
class FtpsServer:
    """FTP with explicit AUTH TLS and PROT P on both control and data sockets."""

    def __init__(self, files, context):
        self.files = files
        self.context = context
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
        except (OSError, ValueError, ssl.SSLError):
            pass
        finally:
            try:
                conn.close()
            except OSError:
                pass

    def handle(self, conn):
        secure = False
        stream = conn.makefile('rwb')
        listening = None
        rest = 0
        data_secure = False

        def reply(line):
            stream.write((line + '\r\n').encode())
            stream.flush()

        def listener():
            sock = socket.socket()
            sock.bind(('127.0.0.1', 0))
            sock.listen(1)
            return sock

        reply('220 odm test ftps ready')
        while True:
            line = stream.readline()
            if not line:
                return
            verb, _, arg = line.decode('latin-1').strip().partition(' ')
            verb = verb.upper()
            if verb == 'AUTH':
                reply('234 switching to TLS')
                conn = self.context.wrap_socket(conn, server_side=True)
                stream = conn.makefile('rwb')
                secure = True
                data_secure = True
            elif verb == 'PBSZ':
                reply('200 PBSZ=0')
            elif verb == 'PROT':
                reply('200 protection level set')
            elif verb == 'USER':
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
                    if data_secure:
                        data = self.context.wrap_socket(data, server_side=True)
                    for i in range(rest, len(blob), 8192):
                        data.sendall(blob[i:min(i + 8192, len(blob))])
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


# ─── MMSH ────────────────────────────────────────────────────────────────────
pragmas = []


class MmsHandler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        pragmas.append(self.headers.get('Pragma', ''))
        self.send_response(200)
        self.send_header('Content-Type', 'application/octet-stream')
        self.send_header('Content-Length', str(len(asf_payload)))
        self.end_headers()
        for i in range(0, len(asf_payload), 8192):
            self.wfile.write(asf_payload[i:min(i + 8192, len(asf_payload))])

    def log_message(self, *args):
        pass


# ─── RTMP ────────────────────────────────────────────────────────────────────
def amf_string(text):
    raw = text.encode()
    return b'\x02' + len(raw).to_bytes(2, 'big') + raw


def amf_number(value):
    return b'\x00' + struct.pack('>d', value)


def amf_null():
    return b'\x05'


def amf_object(props):
    out = b'\x03'
    for key, value in props:
        out += len(key).to_bytes(2, 'big') + key.encode() + value
    return out + b'\x00\x00\x09'


video = b'\x17\x00\x00\x00\x00' + b'VIDEO-FRAMES'
audio = b'\xaf\x01' + b'AUDIO-FRAMES'
metadata = amf_string('onMetaData') + b'\x08' + (3).to_bytes(4, 'big') + \
    b'\x00\x08duration' + amf_number(5) + b'\x00\x05width' + amf_number(320) + \
    b'\x00\x06height' + amf_number(240) + b'\x00\x00\x09'


def chunk(csid, mtype, stream, body, timestamp=0, size=128):
    out = bytearray([csid & 0x3F])
    out += timestamp.to_bytes(3, 'big')
    out += len(body).to_bytes(3, 'big')
    out.append(mtype)
    out += stream.to_bytes(4, 'little')
    offset = 0
    while offset < len(body):
        out += body[offset:offset + size]
        offset += size
        if offset < len(body):
            out.append(0xC0 | (csid & 0x3F))
    return bytes(out)


class RtmpPeer:
    """The server half of RTMP: handshake echo, AMF0 commands, then media."""

    def __init__(self):
        self.buf = bytearray()
        self.out = bytearray()
        self.echo = 1536  # C2, not validated
        self.stream_id = 1
        self.played = False
        self.closed = False

    def handshake(self):
        s1 = b'\x00\x00\x00\x00' + b'\x00\x00\x00\x00' + os.urandom(1528)
        return b'\x03' + s1 + s1

    def feed(self, data):
        self.buf += data
        if self.echo:
            take = min(self.echo, len(self.buf))
            del self.buf[:take]
            self.echo -= take
            if self.echo:
                return
        while len(self.buf) >= 12:
            fmt = self.buf[0] >> 6
            csid = self.buf[0] & 0x3F
            if fmt != 0 or csid == 0 or csid == 1:
                raise RuntimeError('peer only expects small type-0 messages')
            length = int.from_bytes(self.buf[4:7], 'big')
            mtype = self.buf[7]
            stream = int.from_bytes(self.buf[8:12], 'little')
            if len(self.buf) < 12 + length:
                return
            body = bytes(self.buf[12:12 + length])
            del self.buf[:12 + length]
            self.handle(mtype, stream, body)

    def handle(self, mtype, stream, body):
        if mtype != 20:
            return
        if body[0] != 2:
            return
        name = body[3:3 + int.from_bytes(body[1:3], 'big')].decode()
        if name == 'connect':
            self.out += chunk(3, 20, 0, amf_string('_result') + amf_number(1) + amf_null() +
                              amf_object([('fmsVer', amf_string('FMS/3,0,1,123')),
                                          ('capabilities', amf_number(31))]))
        elif name == 'createStream':
            self.out += chunk(3, 20, 0, amf_string('_result') + amf_number(2) + amf_null() + amf_number(1))
        elif name == 'play':
            self.played = True
            self.serve_media(stream or self.stream_id)

    def serve_media(self, stream):
        self.out += chunk(5, 20, stream,
                          amf_string('onStatus') + amf_number(0) + amf_null() +
                          amf_object([('level', amf_string('status')),
                                      ('code', amf_string('NetStream.Play.Start'))]))
        self.out += chunk(5, 18, stream, metadata, timestamp=0)
        self.out += chunk(5, 9, stream, video, timestamp=40)
        self.out += chunk(5, 8, stream, audio, timestamp=60)
        self.out += chunk(5, 20, stream,
                          amf_string('onStatus') + amf_number(0) + amf_null() +
                          amf_object([('level', amf_string('status')),
                                      ('code', amf_string('NetStream.Play.Stop'))]))
        self.closed = True

    def drain(self):
        out = bytes(self.out)
        self.out.clear()
        return out


class RtmpServer:
    def __init__(self):
        self.sock = socket.socket()
        self.sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self.sock.bind(('127.0.0.1', 0))
        self.sock.listen(4)
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
        peer = RtmpPeer()
        try:
            need = 1537
            data = b''
            while len(data) < need:
                block = conn.recv(4096)
                if not block:
                    return
                data += block
            conn.sendall(peer.handshake())
            peer.echo = 1536
            peer.feed(data[1537:])
            if peer.out:
                conn.sendall(peer.drain())
            while True:
                block = conn.recv(65536)
                if not block:
                    return
                peer.feed(block)
                if peer.out:
                    conn.sendall(peer.drain())
                if peer.closed and not peer.out:
                    time.sleep(0.1)
                    return
        except (OSError, RuntimeError):
            pass
        finally:
            try:
                conn.close()
            except OSError:
                pass


class TunnelHandler(http.server.BaseHTTPRequestHandler):
    peers = {}
    lock = threading.Lock()
    session = {'peer': None}

    def do_POST(self):
        length = int(self.headers.get('Content-Length', '0'))
        body = self.rfile.read(length) if length else b''
        with self.lock:
            if self.path.startswith('/open'):
                peer = RtmpPeer()
                # The tunnel keeps the RTMP handshake inside /send bodies:
                # skip C0+C1 and the C2 echo before parsing chunk headers.
                peer.echo = 1537 + 1536
                self.session['peer'] = peer
                reply = peer.handshake()
                self.respond(reply)
                return
            peer = self.session['peer']
            if peer is None:
                self.respond(b'', status=404)
                return
            peer.feed(body)
            self.respond(peer.drain())

    def respond(self, body, status=200):
        self.send_response(status)
        self.send_header('Content-Type', 'application/x-fcs')
        self.send_header('Content-Length', str(len(body)))
        self.send_header('Connection', 'keep-alive')
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


def flv_tags(path):
    data = pathlib.Path(path).read_bytes()
    expect(data[:3] == b'FLV', f'{path}: missing FLV signature')
    expect(data[4] == 5, f'{path}: FLV flags should announce audio+video, got {data[4]}')
    at = int.from_bytes(data[5:9], 'big') + 4  # header, then PreviousTagSize0
    tags = []
    while at + 11 <= len(data):
        mtype = data[at]
        size = int.from_bytes(data[at + 1:at + 4], 'big')
        stamp = int.from_bytes(data[at + 4:at + 7], 'big') | (data[at + 7] << 24)
        body = data[at + 11:at + 11 + size]
        previous = int.from_bytes(data[at + 11 + size:at + 15 + size], 'big')
        expect(previous == size + 11, f'{path}: wrong PreviousTagSize {previous} for {size}')
        tags.append((mtype, stamp, body))
        at += 15 + size
    return tags


with tempfile.TemporaryDirectory() as tmp:
    # ftps://
    openssl = shutil.which('openssl')
    if openssl:
        cert = pathlib.Path(tmp) / 'cert.pem'
        key = pathlib.Path(tmp) / 'key.pem'
        conf = pathlib.Path(tmp) / 'openssl.cnf'
        # The distro openssl may point OPENSSL_CONF at a config that is not on this
        # machine; an empty config keeps req working with only -subj/-addext.
        conf.write_text('')
        subprocess.run([openssl, 'req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '1',
                        '-config', str(conf),
                        '-keyout', str(key), '-out', str(cert), '-subj', '/CN=127.0.0.1',
                        '-addext', 'subjectAltName=IP:127.0.0.1'],
                       check=True, capture_output=True)
        context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        context.load_cert_chain(str(cert), str(key))
        ftps = FtpsServer({'secure.bin': payload}, context)
        destination = pathlib.Path(tmp) / 'ftps.bin'
        env = dict(os.environ, ODM_TLS_INSECURE='1')
        subprocess.run([exe, f'ftps://127.0.0.1:{ftps.port}/secure.bin', destination],
                       check=True, timeout=60, env=env)
        expect(destination.read_bytes() == payload, 'ftps:// payload is not byte-exact')
        expect(not pathlib.Path(str(destination) + '.odm-work').exists(), 'ftps:// left staging behind')
        ftps.sock.close()
    else:
        print('SKIP ftps:// (openssl not on PATH)')

    # mmsh://
    with http.server.ThreadingHTTPServer(('127.0.0.1', 0), MmsHandler) as server:
        threading.Thread(target=server.serve_forever, daemon=True).start()
        mms = pathlib.Path(tmp) / 'stream.asf'
        subprocess.run([exe, f'mmsh://127.0.0.1:{server.server_port}/stream.wmv', mms], check=True, timeout=60)
        expect(mms.read_bytes() == asf_payload, 'mmsh:// payload is not byte-exact')
        expect(any('xClientGUID' in pragma for pragma in pragmas), f'MMSH tunnel header missing: {pragmas}')
        server.shutdown()

    # rtmp://
    rtmp = RtmpServer()
    flv = pathlib.Path(tmp) / 'capture.flv'
    subprocess.run([exe, f'rtmp://127.0.0.1:{rtmp.port}/live/demo', flv], check=True, timeout=60)
    tags = flv_tags(flv)
    kinds = [kind for kind, _, _ in tags]
    expect(18 in kinds and 9 in kinds and 8 in kinds, f'rtmp:// FLV tag types {kinds}')
    expect([body for kind, _, body in tags if kind == 9] == [video], 'rtmp:// video payload mismatch')
    expect([body for kind, _, body in tags if kind == 8] == [audio], 'rtmp:// audio payload mismatch')
    expect([body for kind, _, body in tags if kind == 18] == [metadata], 'rtmp:// metadata payload mismatch')
    stamps = {kind: stamp for kind, stamp, _ in tags}
    expect(stamps.get(9) == 40 and stamps.get(8) == 60, f'rtmp:// timestamps {stamps}')
    rtmp.sock.close()

    # rtmpt://
    tunnel = http.server.ThreadingHTTPServer(('127.0.0.1', 0), TunnelHandler)
    threading.Thread(target=tunnel.serve_forever, daemon=True).start()
    tunnelled = pathlib.Path(tmp) / 'tunnel.flv'
    subprocess.run([exe, f'rtmpt://127.0.0.1:{tunnel.server_port}/live/demo', tunnelled], check=True, timeout=60)
    tunnel_tags = flv_tags(tunnelled)
    tunnel_kinds = [kind for kind, _, _ in tunnel_tags]
    expect(9 in tunnel_kinds and 8 in tunnel_kinds, f'rtmpt:// FLV tag types {tunnel_kinds}')
    tunnel.shutdown()

if failures:
    print(f'{len(failures)} failure(s)')
    sys.exit(1)
print('stream protocols ok')
