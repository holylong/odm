package engine

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"odm/internal/config"
)

// payload builds a deterministic body so a checksum comparison is meaningful.
func payload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*31 + i/251) % 251)
	}
	return b
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// rangeServer serves body with full byte-range support, the shape IDM requires
// for splitting: 206 + Content-Range + Accept-Ranges.
func rangeServer(t *testing.T, body []byte, allowRange bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			if allowRange {
				w.Header().Set("Accept-Ranges", "bytes")
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(200)
			return
		}
		if allowRange {
			w.Header().Set("Accept-Ranges", "bytes")
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		rng := r.Header.Get("Range")
		if allowRange && rng != "" {
			start, end, ok := parseRange(rng, len(body))
			if !ok {
				w.WriteHeader(416)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(206)
			_, _ = w.Write(body[start : end+1])
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(200)
		_, _ = w.Write(body)
	}))
}

func parseRange(h string, size int) (int64, int64, bool) {
	if !strings.HasPrefix(h, "bytes=") {
		return 0, 0, false
	}
	parts := strings.SplitN(strings.TrimPrefix(h, "bytes="), "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	start, err1 := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
	end := int64(size - 1)
	var err2 error
	if strings.TrimSpace(parts[1]) != "" {
		end, err2 = strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	}
	if err1 != nil || err2 != nil || start < 0 || end >= int64(size) || start > end {
		return 0, 0, false
	}
	return start, end, true
}

// sandbox points the settings at per-test directories.
func sandbox(t *testing.T) (downloadDir, tempDir string) {
	t.Helper()
	downloadDir = t.TempDir()
	tempDir = t.TempDir()
	s := config.Default()
	s.DefaultDir = downloadDir
	s.TempDir = tempDir
	s.MaxConnectionsNumber = 4
	s.MinSegmentSize = 64 * 1024
	s.TimeoutSec = 10
	s.MaxRetries = 3
	config.Set(s)
	return downloadDir, tempDir
}

// waitComplete polls the task until it leaves the active states.
func waitComplete(t *testing.T, m *Manager, id string, limit time.Duration) *Task {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		tk := m.Get(id)
		if tk != nil {
			tk.mu.Lock()
			st := tk.Status
			tk.mu.Unlock()
			switch st {
			case StatusComplete:
				return tk
			case StatusError:
				tk.mu.Lock()
				err := tk.Error
				tk.mu.Unlock()
				t.Fatalf("download failed: %s", err)
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	tk := m.Get(id)
	if tk != nil {
		tk.mu.Lock()
		defer tk.mu.Unlock()
		t.Fatalf("timeout waiting for completion; status=%s downloaded=%d/%d", tk.Status, tk.RecomputeDownloaded(), tk.Size)
	}
	t.Fatal("task disappeared")
	return nil
}

// TestSegmentedDownload proves the split path: one file, many Range requests,
// byte-exact output.
func TestSegmentedDownload(t *testing.T) {
	downloadDir, tempDir := sandbox(t)
	body := payload(1024 * 1024)
	srv := rangeServer(t, body, true)
	defer srv.Close()

	mgr := NewManager(tempDir, nil)
	task, err := mgr.Add(AddOptions{URL: srv.URL + "/payload.bin", Filename: "payload.bin", StartNow: true})
	if err != nil {
		t.Fatal(err)
	}
	done := waitComplete(t, mgr, task.ID, 30*time.Second)

	done.mu.Lock()
	segs := len(done.Segments)
	size := done.Size
	done.mu.Unlock()

	if size != int64(len(body)) {
		t.Fatalf("size = %d, want %d", size, len(body))
	}
	if segs < 2 {
		t.Fatalf("expected at least 2 segments, got %d", segs)
	}

	got, err := os.ReadFile(filepath.Join(downloadDir, "payload.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("content mismatch: got %d bytes sha=%s want %d bytes sha=%s",
			len(got), sum(got), len(body), sum(body))
	}
	t.Logf("segmented download ok: %d segments, %d bytes, sha256=%s", segs, len(got), sum(got))
}

// TestResumeAfterPause proves the resume record round-trips: stop mid-flight,
// reload the manager from disk, resume, and still land on identical bytes.
func TestResumeAfterPause(t *testing.T) {
	downloadDir, tempDir := sandbox(t)
	body := payload(2 * 1024 * 1024)
	srv := rangeServer(t, body, true)
	defer srv.Close()

	mgr := NewManager(tempDir, nil)
	task, err := mgr.Add(AddOptions{URL: srv.URL + "/big.bin", Filename: "big.bin", StartNow: true})
	if err != nil {
		t.Fatal(err)
	}

	// Let it get going, then pause hard.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		task.mu.Lock()
		got := task.RecomputeDownloaded()
		task.mu.Unlock()
		if got > 64*1024 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mgr.Pause(task.ID)
	time.Sleep(200 * time.Millisecond)

	task.mu.Lock()
	pausedAt := task.RecomputeDownloaded()
	task.mu.Unlock()
	if pausedAt == 0 {
		t.Fatal("nothing was downloaded before the pause; cannot test resume")
	}
	t.Logf("paused at %d of %d bytes", pausedAt, len(body))

	// A fresh manager must reload the same resume record from disk.
	mgr2 := NewManager(tempDir, nil)
	reloaded := mgr2.Get(task.ID)
	if reloaded == nil {
		t.Fatal("resume record was not persisted")
	}
	reloaded.mu.Lock()
	segs := len(reloaded.Segments)
	resumeOffsets := reloaded.SegmentStarts()
	reloaded.mu.Unlock()
	if segs < 2 {
		t.Fatalf("expected a segmented resume record, got %d segments", segs)
	}
	t.Logf("reloaded %d segments, starts=%v", segs, resumeOffsets)

	if err := mgr2.Resume(task.ID); err != nil {
		t.Fatal(err)
	}
	waitComplete(t, mgr2, task.ID, 60*time.Second)

	got, err := os.ReadFile(filepath.Join(downloadDir, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("resumed content mismatch: got %d bytes sha=%s want %d bytes sha=%s",
			len(got), sum(got), len(body), sum(body))
	}
	t.Logf("resume ok: %d bytes, sha256=%s", len(got), sum(got))
}

// TestNoRangeFallsBackToSingleConnection proves the degrade path when a server
// ignores Range: the file must still arrive intact on one connection.
func TestNoRangeFallsBackToSingleConnection(t *testing.T) {
	downloadDir, tempDir := sandbox(t)
	body := payload(300 * 1024)
	srv := rangeServer(t, body, false)
	defer srv.Close()

	mgr := NewManager(tempDir, func(f string, a ...any) { t.Logf("[mgr] "+f, a...) })
	task, err := mgr.Add(AddOptions{URL: srv.URL + "/plain.bin", Filename: "plain.bin", StartNow: true})
	if err != nil {
		t.Fatal(err)
	}
	done := waitComplete(t, mgr, task.ID, 30*time.Second)

	done.mu.Lock()
	single := done.SingleConn
	conns := done.Connections
	done.mu.Unlock()
	if !single {
		t.Fatalf("expected single-connection mode, got %d connections", conns)
	}

	got, err := os.ReadFile(filepath.Join(downloadDir, "plain.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("content mismatch on single-connection download")
	}
	t.Logf("single-connection fallback ok: %d bytes", len(got))
}

// TestSegmentsSplitUntilMinimum checks the splitter honours MinSegmentSize.
func TestSegmentsSplitUntilMinimum(t *testing.T) {
	segs := NewSegments(1024 * 1024)
	for i := 0; i < 64; i++ {
		if _, _, ok := segs.SplitLargest(256 * 1024); !ok {
			break
		}
	}
	list := segs.List()
	// The splitter appends new halves instead of inserting them, so compare the
	// set spatially rather than by slice order.
	sort.Slice(list, func(i, j int) bool { return list[i].Start < list[j].Start })
	var covered int64
	var prevEnd int64 = -1
	for _, s := range list {
		if s.Start != prevEnd+1 {
			t.Fatalf("segments are not contiguous: prev end %d, next start %d", prevEnd, s.Start)
		}
		covered += s.End - s.Start + 1
		prevEnd = s.End
	}
	if covered != 1024*1024 {
		t.Fatalf("segments cover %d bytes, want %d", covered, 1024*1024)
	}
	for _, s := range list {
		if s.End-s.Start+1 < 256*1024 && len(list) > 1 {
			t.Fatalf("segment below the minimum size: %d", s.End-s.Start+1)
		}
	}
	t.Logf("%d segments covering %d bytes", len(list), covered)
}

// TestFilenameSanitising covers the path-safety rules applied to server names.
func TestFilenameSanitising(t *testing.T) {
	cases := map[string]string{
		`../../etc/passwd`: "passwd",
		`C:\windows\x.exe`: "x.exe",
		`a/b/c.tar.gz`:     "c.tar.gz",
		``:                 "",
		`con`:              "_con",
		`..`:               "",
		`nul.txt`:          "_nul.txt",
		`report.docx`:      "report.docx",
	}
	for in, want := range cases {
		if got := SanitizeFilename(in); got != want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
}
