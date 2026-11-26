package httpx

import (
	"errors"
	"io"
	"strconv"
	"strings"
)

// Read streams the response body, decoding chunked transfer-encoding when used.
func (r *Response) Read(p []byte) (int, error) {
	if r.eof {
		return 0, io.EOF
	}
	if r.noBody {
		r.eof = true
		return 0, io.EOF
	}
	if r.chunked {
		return r.readChunked(p)
	}
	if r.remain < 0 {
		n, err := r.br.Read(p)
		if err == io.EOF {
			r.eof = true
			r.br = nil
		}
		return n, err
	}
	if r.remain == 0 {
		r.eof = true
		r.br = nil
		return 0, io.EOF
	}
	if int64(len(p)) > r.remain {
		p = p[:r.remain]
	}
	n, err := r.br.Read(p)
	r.remain -= int64(n)
	if err == io.EOF {
		if r.remain > 0 {
			return n, io.ErrUnexpectedEOF
		}
		r.eof = true
		r.br = nil
		return n, io.EOF
	}
	if r.remain == 0 {
		r.eof = true
	}
	return n, err
}

func (r *Response) readChunked(p []byte) (int, error) {
	if r.remain == 0 {
		// read CRLF after chunk data
		if _, err := r.br.ReadString('\n'); err != nil {
			return 0, err
		}
		line, err := r.br.ReadString('\n')
		if err != nil {
			return 0, err
		}
		sizeStr := strings.TrimRight(line, "\r\n")
		if i := strings.IndexByte(sizeStr, ';'); i >= 0 {
			sizeStr = sizeStr[:i]
		}
		size, perr := strconv.ParseInt(strings.TrimSpace(sizeStr), 16, 64)
		if perr != nil {
			return 0, errors.New("httpx: bad chunk size " + sizeStr)
		}
		if size == 0 {
			// trailers
			for {
				l, err := r.br.ReadString('\n')
				if err != nil || l == "\r\n" || l == "\n" {
					break
				}
			}
			r.eof = true
			return 0, io.EOF
		}
		r.remain = size
	}
	if int64(len(p)) > r.remain {
		p = p[:r.remain]
	}
	n, err := r.br.Read(p)
	r.remain -= int64(n)
	return n, err
}

// Close finishes the body. A fully drained body on a Keep-Alive response returns
// the socket to the pool so the next segment can skip TCP/TLS setup — the
// "connection reuse" behaviour described in re/FINDINGS.md §8.
func (r *Response) Close() error {
	fullyDrained := r.eof || (r.remain == 0 && !r.chunked)
	if !fullyDrained {
		// Drain up to a small bound; beyond that the socket is not worth keeping.
		n, _ := io.CopyN(io.Discard, r, 64*1024)
		fullyDrained = r.eof || n < 64*1024
	}
	if r.cn == nil {
		return nil
	}
	if !fullyDrained || r.br == nil {
		if r.client != nil {
			r.client.closeConn(r.cn, "response body not drained")
		} else {
			_ = r.cn.c.Close()
		}
		r.cn = nil
		return nil
	}
	r.cn.pooled = true
	if c := r.client; c != nil {
		c.release(r.cn)
	}
	r.cn = nil
	return nil
}

func (r *Response) drain() error { return r.Close() }
