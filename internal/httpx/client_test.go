package httpx

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"odm/internal/config"
)

func testBody(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*17 + 3) % 253)
	}
	return b
}

// serveNoRange returns the whole body regardless of any Range header, the way a
// server without range support behaves.
func serveNoRange(t *testing.T, body []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(200)
		_, _ = w.Write(body)
	}))
}

func newTestClient(t *testing.T) *Client {
	t.Helper()
	return New(Options{
		Timeout:   10 * time.Second,
		UserAgent: "odm-test",
		MaxIdlePerHost: 4,
		Logf:      func(f string, a ...any) { t.Logf(f, a...) },
	})
}

// TestProbeThenFullRead reproduces the probe-then-download sequence: a one-byte
// ranged probe whose body the server ignores, then a full read. The probe must
// not hand a half-read socket to the pool.
func TestProbeThenFullRead(t *testing.T) {
	body := testBody(300 * 1024)
	srv := serveNoRange(t, body)
	defer srv.Close()
	u, _ := url.Parse(srv.URL + "/plain.bin")

	c := newTestClient(t)
	defer c.Close()

	// Probe: ask for one byte, read one byte, close.
	p, err := c.Do(&Request{
		Method: "GET", URL: u,
		Range:            &Range{Start: 0, End: 0, HasEnd: true},
		IdentityEncoding: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	one := make([]byte, 1)
	if _, err := p.Read(one); err != nil {
		t.Fatalf("probe read: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("probe close: %v", err)
	}

	// Full read on the same client.
	r, err := c.Do(&Request{
		Method: "GET", URL: u,
		Range:            &Range{Start: 0, End: int64(len(body) - 1), HasEnd: true},
		IdentityEncoding: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("full read after probe: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), body) {
		t.Fatalf("body mismatch: got %d bytes, want %d", buf.Len(), len(body))
	}
	t.Logf("full read after probe ok: %d bytes", buf.Len())
}

// TestKeepAliveReusesSocket checks a drained response returns its socket and the
// next request works on it (range server, two segments back to back).
func TestKeepAliveReusesSocket(t *testing.T) {
	body := testBody(64 * 1024)
	var served int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		rng := r.Header.Get("Range")
		if rng == "" {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			_, _ = w.Write(body)
			return
		}
		var s, e int
		fmt.Sscanf(rng, "bytes=%d-%d", &s, &e)
		if e >= len(body) {
			e = len(body) - 1
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", s, e, len(body)))
		w.Header().Set("Content-Length", strconv.Itoa(e-s+1))
		w.WriteHeader(206)
		_, _ = w.Write(body[s : e+1])
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL + "/two.bin")

	c := newTestClient(t)
	defer c.Close()

	out := make([]byte, 0, len(body))
	for i := 0; i < 2; i++ {
		s := i * 32 * 1024
		e := s + 32*1024 - 1
		r, err := c.Do(&Request{Method: "GET", URL: u, Range: &Range{Start: int64(s), End: int64(e), HasEnd: true}})
		if err != nil {
			t.Fatalf("segment %d: %v", i, err)
		}
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(r); err != nil {
			t.Fatalf("segment %d read: %v", i, err)
		}
		_ = r.Close()
		out = append(out, buf.Bytes()...)
	}
	if !bytes.Equal(out, body) {
		t.Fatalf("assembled body mismatch: got %d bytes", len(out))
	}
	t.Logf("two ranged reads ok, server saw %d requests", served)
}

var _ = config.Default
