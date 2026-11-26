package engine

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"odm/internal/config"
	"odm/internal/httpx"
)

// ErrStopped is returned when a download is paused or cancelled.
var ErrStopped = errors.New("stopped")

// Limiter is a token bucket shared by every connection of one task.
type Limiter struct {
	mu     sync.Mutex
	rate   int64 // bytes/second, 0 = unlimited
	tokens float64
	last   time.Time
}

// NewLimiter builds a limiter at rateKBps kilobytes/second (0 disables).
func NewLimiter(rateKBps int) *Limiter {
	if rateKBps <= 0 {
		return nil
	}
	return &Limiter{rate: int64(rateKBps) * 1024}
}

// Throttle blocks as needed so the long-run rate stays at the limit.
func (l *Limiter) Throttle(n int) {
	if l == nil || l.rate <= 0 || n <= 0 {
		return
	}
	l.mu.Lock()
	now := time.Now()
	if l.last.IsZero() {
		l.last = now
		l.tokens = float64(l.rate) / 10 // small burst allowance
	}
	l.tokens += now.Sub(l.last).Seconds() * float64(l.rate)
	l.last = now
	if max := float64(l.rate); l.tokens > max {
		l.tokens = max
	}
	l.tokens -= float64(n)
	var sleep time.Duration
	if l.tokens < 0 {
		sleep = time.Duration(-l.tokens / float64(l.rate) * float64(time.Second))
		l.tokens = 0
	}
	l.mu.Unlock()
	if sleep > 0 {
		time.Sleep(sleep)
	}
}

// Downloader runs one task. Two locks are held, always in the order
// segMu -> Task.mu, so a worker can publish progress without racing a reader.
type Downloader struct {
	Task   *Task
	Client *httpx.Client
	Logf   func(format string, args ...any)

	segMu   sync.Mutex
	segs    *Segments
	file    *os.File
	limiter *Limiter
	workers int
	minSeg  int64

	stop    chan struct{}
	stopped atomic.Bool
	wg      sync.WaitGroup
}

// NewDownloader wires a task to a client.
func NewDownloader(t *Task, c *httpx.Client, logf func(string, ...any)) *Downloader {
	s := config.Current()
	return &Downloader{
		Task: t, Client: c, Logf: logf,
		limiter: NewLimiter(s.SpeedLimitKBps),
		minSeg:  s.MinSegmentSize,
		workers: s.MaxConnectionsNumber,
		stop:    make(chan struct{}),
	}
}

// Stop asks every worker to finish.
func (d *Downloader) Stop() {
	if d.stopped.Swap(true) {
		return
	}
	close(d.stop)
}

func (d *Downloader) isStopped() bool {
	select {
	case <-d.stop:
		return true
	default:
		return false
	}
}

func (d *Downloader) logf(f string, a ...any) {
	if d.Logf != nil {
		d.Logf(f, a...)
	}
}

// ------------------------------------------------------------ segment access

// claim hands worker a segment: a free one if available, otherwise the second
// half of the largest segment (the documented in-half division rule).
func (d *Downloader) claim(worker int) (int, bool) {
	d.segMu.Lock()
	defer d.segMu.Unlock()
	if idx, ok := d.segs.ClaimFree(worker); ok {
		return idx, true
	}
	idx, src, ok := d.segs.SplitLargest(d.minSeg)
	if !ok {
		return 0, false
	}
	list := d.segs.List()
	list[idx].Claim = worker
	d.logf("split: [%d-%d] -> [%d-%d] + [%d-%d]",
		list[src].Start, list[idx].End, list[src].Start, list[src].End, list[idx].Start, list[idx].End)
	return idx, true
}

func (d *Downloader) bounds(idx int) (start, end, pos int64, done bool) {
	d.segMu.Lock()
	defer d.segMu.Unlock()
	s := d.segs.List()[idx]
	return s.Start, s.End, s.Pos, s.Done
}

func (d *Downloader) setPos(idx int, pos int64) {
	d.segMu.Lock()
	d.segs.List()[idx].Pos = pos
	d.segMu.Unlock()
}

func (d *Downloader) markDone(idx int) {
	d.segMu.Lock()
	s := &d.segs.List()[idx]
	s.Pos = s.End + 1
	s.Done = true
	s.Claim = 0
	d.segMu.Unlock()
}

func (d *Downloader) allDone() bool {
	d.segMu.Lock()
	defer d.segMu.Unlock()
	return d.segs.AllDone()
}

func (d *Downloader) resetSegments(size int64) {
	d.segMu.Lock()
	d.segs.Reset(size)
	d.segMu.Unlock()
}

// syncTask publishes the segment table into the Task so readers (UI, resume
// record) see consistent numbers.
func (d *Downloader) syncTask() {
	d.segMu.Lock()
	segs := make([]Segment, len(d.segs.List()))
	copy(segs, d.segs.List())
	d.segMu.Unlock()

	d.Task.mu.Lock()
	d.Task.Segments = segs
	d.Task.Downloaded = d.Task.RecomputeDownloaded()
	d.Task.mu.Unlock()
}

// ---------------------------------------------------------------- lifecycle

// Run probes and downloads until finished, stopped, or failed.
func (d *Downloader) Run() error {
	t := d.Task
	if d.IsFTP() {
		if err := d.runFTP(); err != nil {
			if d.isStopped() {
				return ErrStopped
			}
			return err
		}
		return nil
	}
	if err := d.probe(); err != nil {
		if d.isStopped() {
			return ErrStopped
		}
		return err
	}
	if err := d.openPart(); err != nil {
		return err
	}
	defer d.closePart()

	if err := d.download(); err != nil {
		d.syncTask()
		_ = t.Save()
		return err
	}
	if err := d.assemble(); err != nil {
		return err
	}
	return nil
}

// probe discovers size, resume support, file name and session cookies. It uses a
// one-byte ranged GET: a 206 with Content-Range yields the total size, a 200
// means the server ignores ranges and only Content-Length is trustworthy.
func (d *Downloader) probe() error {
	t := d.Task
	t.mu.Lock()
	t.Status = StatusProbing
	t.mu.Unlock()

	u, err := url.Parse(t.URL)
	if err != nil {
		return fmt.Errorf("bad URL: %w", err)
	}
	resp, err := d.Client.Do(&httpx.Request{
		Method:           "GET",
		URL:              u,
		Range:            &httpx.Range{Start: 0, End: 0, HasEnd: true},
		Header:           mapToHeader(t.Headers),
		Referer:          t.Referer,
		Cookies:          t.Cookies,
		IdentityEncoding: true,
	})
	if err != nil {
		return fmt.Errorf("cannot connect to the server: %w", err)
	}
	defer resp.Close()
	d.absorbCookies(resp, u.Hostname())

	var size int64 = -1
	resume := false
	switch resp.StatusCode {
	case 206:
		if cr := resp.ContentRange; cr != nil && cr.HasTotal {
			size = cr.Total
		}
		resume = true
	case 200:
		size = resp.ContentLength
		resume = resp.AcceptRanges
	case 416:
		return errors.New("server returned 416 for a one-byte probe")
	case 401, 403:
		return fmt.Errorf("server returned %d %s: authorization failed", resp.StatusCode, resp.Reason)
	case 404:
		return fmt.Errorf("server returned 404 %s: file not found", resp.Reason)
	default:
		if resp.StatusCode >= 300 {
			return fmt.Errorf("server returned %d %s", resp.StatusCode, resp.Reason)
		}
	}

	name := t.Filename
	if name == "" {
		if resp.Filename != "" {
			name = SanitizeFilename(resp.Filename)
		}
		if name == "" {
			name = FilenameFromURL(resp.FinalURL.String())
		}
	}
	final := resp.FinalURL
	if final == nil {
		final = u
	}

	t.mu.Lock()
	t.Size = size
	t.ResumeSupport = resume
	t.Filename = name
	t.Mime = resp.Header.Get("Content-Type")
	t.LastModTime = resp.Header.Get("Last-Modified")
	t.FinalURL = final.String()
	t.mu.Unlock()

	d.logf("probe: %d size=%d resume=%v name=%q", resp.StatusCode, size, resume, name)
	if size > 0 && !resume {
		d.logf("server does not support resume; one connection from zero")
	}
	return nil
}

func (d *Downloader) absorbCookies(resp *httpx.Response, host string) {
	var out []httpx.Cookie
	for _, sc := range resp.Header.Values("Set-Cookie") {
		first := sc
		if i := strings.IndexByte(first, ';'); i >= 0 {
			first = first[:i]
		}
		kv := strings.SplitN(strings.TrimSpace(first), "=", 2)
		if len(kv) != 2 || kv[0] == "" {
			continue
		}
		out = append(out, httpx.Cookie{Name: kv[0], Value: kv[1]})
	}
	if len(out) == 0 {
		return
	}
	d.Client.SetCookies(host, out)
	d.Task.mu.Lock()
	d.Task.Cookies = append(d.Task.Cookies, out...)
	d.Task.mu.Unlock()
}

func (d *Downloader) openPart() error {
	t := d.Task
	if err := os.MkdirAll(filepath.Dir(t.TempPath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(t.TempPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	d.file = f

	t.mu.Lock()
	size := t.Size
	existing := t.Segments
	t.mu.Unlock()

	if size > 0 {
		if st, err := f.Stat(); err == nil && st.Size() != size {
			if err := f.Truncate(size); err != nil {
				return fmt.Errorf("cannot size the temporary file: %w", err)
			}
		}
	}
	if len(existing) == 0 {
		if size < 0 {
			size = 0
		}
		d.segs = NewSegments(size)
	} else {
		d.segs = Wrap(existing)
	}
	// Stale claims from a previous run must not block the new workers.
	for i := range d.segs.List() {
		d.segs.List()[i].Claim = 0
	}
	return nil
}

func (d *Downloader) closePart() {
	d.syncTask()
	_ = d.Task.Save()
	if d.file != nil {
		_ = d.file.Close()
		d.file = nil
	}
}

// download decides the worker count and runs the pool.
func (d *Downloader) download() error {
	t := d.Task
	t.mu.Lock()
	t.Status = StatusDownloading
	size := t.Size
	resume := t.ResumeSupport
	single := t.SingleConn
	t.mu.Unlock()

	want := 1
	switch {
	case size <= 0:
		// Unknown size: one streaming connection, exactly like IDM reports
		// "the file size is unknown".
		want = 1
	case !resume || single:
		want = 1
	default:
		want = d.workers
		if d.minSeg > 0 {
			maxBySize := int(size / d.minSeg)
			if maxBySize < 1 {
				maxBySize = 1
			}
			if want > maxBySize {
				want = maxBySize
			}
		}
		if want < 1 {
			want = 1
		}
	}
	if want == 1 && size > 0 && !resume {
		d.resetSegments(size)
	}
	d.workers = want

	t.mu.Lock()
	t.Connections = want
	if want == 1 {
		t.SingleConn = true
	}
	t.mu.Unlock()

	done := make(chan struct{})
	go func() {
		tk := time.NewTicker(time.Second)
		defer tk.Stop()
		for {
			select {
			case <-done:
				return
			case <-d.stop:
				d.syncTask()
				_ = t.Save()
				return
			case <-tk.C:
				d.syncTask()
				_ = t.Save()
			}
		}
	}()

	for i := 1; i <= want; i++ {
		d.wg.Add(1)
		go d.worker(i)
	}
	d.wg.Wait()
	close(done)

	if d.isStopped() {
		return ErrStopped
	}
	if !d.allDone() {
		return errors.New("download incomplete: connections stopped before the last byte")
	}
	return nil
}

// worker claims segments until none are left to claim or split.
func (d *Downloader) worker(id int) {
	defer d.wg.Done()
	for {
		if d.isStopped() {
			return
		}
		idx, ok := d.claim(id)
		if !ok {
			return
		}
		if err := d.fetch(id, idx); err != nil {
			if errors.Is(err, ErrStopped) {
				return
			}
			if errors.Is(err, httpx.ErrNoResume) {
				d.degradeToSingle(err)
				return
			}
			d.Task.mu.Lock()
			if d.Task.Error == "" {
				d.Task.Error = err.Error()
			}
			d.Task.mu.Unlock()
			d.Stop()
			return
		}
	}
}

// fetch downloads segment idx from its current position to its end.
func (d *Downloader) fetch(worker, idx int) error {
	maxRetries := config.Current().MaxRetries
	u, err := url.Parse(d.Task.URL)
	if err != nil {
		return err
	}
	backoff := 250 * time.Millisecond

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if d.isStopped() {
			return ErrStopped
		}
		start, end, pos, isDone := d.bounds(idx)
		if isDone || pos > end {
			d.markDone(idx)
			return nil
		}

		rng := &httpx.Range{Start: pos, End: end, HasEnd: true}
		req := &httpx.Request{
			Method:           "GET",
			URL:              u,
			Range:            rng,
			Header:           mapToHeader(d.Task.Headers),
			Referer:          d.Task.Referer,
			Cookies:          d.Task.Cookies,
			IdentityEncoding: true,
		}
		if d.Task.LastModTime != "" && !config.Current().IgnoreModTimeChange {
			req.Header.Set("If-Modified-Since", d.Task.LastModTime)
		}
		resp, err := d.Client.Do(req)
		if err != nil {
			d.logf("worker %d: %v (retry %d)", worker, err, attempt+1)
			if d.sleepBackoff(backoff) {
				return ErrStopped
			}
			backoff = nextBackoff(backoff)
			continue
		}
		d.absorbCookies(resp, u.Hostname())

		switch resp.StatusCode {
		case 206:
			// Ranged response, as required.
		case 200:
			// The server ignored Range. From a non-zero offset the transfer cannot
			// be segmented; from zero the whole body is exactly what we want, so
			// stream it on this one connection.
			if start > 0 || pos > 0 {
				_ = resp.Close()
				return fmt.Errorf("%w (server sent the whole file for Range %s)",
					httpx.ErrNoResume, rng.String())
			}
		case 416:
			_ = resp.Close()
			d.logf("worker %d: 416 for %s, segment complete", worker, rng.String())
			d.markDone(idx)
			return nil
		case 429, 503:
			wait := httpx.ParseRetryAfter(resp.Header.Get("Retry-After"))
			_ = resp.Close()
			if wait > 0 {
				d.logf("worker %d: retry-after %ds", worker, wait)
				if d.sleepBackoff(time.Duration(wait) * time.Second) {
					return ErrStopped
				}
			} else if d.sleepBackoff(backoff) {
				return ErrStopped
			}
			backoff = nextBackoff(backoff)
			continue
		default:
			code, reason := resp.StatusCode, resp.Reason
			_ = resp.Close()
			return fmt.Errorf("server returned %d %s", code, reason)
		}

		buf := make([]byte, 64*1024)
		broken := false
		for {
			if d.isStopped() {
				_ = resp.Close()
				return ErrStopped
			}
			n, rerr := resp.Read(buf)
			if n > 0 {
				if pos+int64(n)-1 > end {
					n = int(end - pos + 1)
				}
				if n > 0 {
					if _, werr := d.file.WriteAt(buf[:n], pos); werr != nil {
						_ = resp.Close()
						return fmt.Errorf("cannot write to the temporary file: %w", werr)
					}
					pos += int64(n)
					d.setPos(idx, pos)
					d.limiter.Throttle(n)
				}
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				d.logf("worker %d: %v at %d, reconnecting", worker, rerr, pos)
				broken = true
				break
			}
			if pos > end {
				break
			}
		}
		_ = resp.Close()

		if pos > end {
			d.markDone(idx)
			return nil
		}
		_ = broken
		if d.sleepBackoff(backoff) {
			return ErrStopped
		}
		backoff = nextBackoff(backoff)
	}
	start, end, _, _ := d.bounds(idx)
	return fmt.Errorf("giving up on segment [%d-%d] after %d retries", start, end, maxRetries)
}

func (d *Downloader) sleepBackoff(dur time.Duration) bool {
	select {
	case <-d.stop:
		return true
	case <-time.After(dur):
		return false
	}
}

func nextBackoff(b time.Duration) time.Duration {
	b *= 2
	if b > 8*time.Second {
		b = 8 * time.Second
	}
	return b
}

// degradeToSingle implements the observed fallback: when segmented transfers
// cannot be reconciled, restart from zero on one connection.
func (d *Downloader) degradeToSingle(cause error) {
	d.logf("degrading to a single connection: %v", cause)
	d.Task.mu.Lock()
	size := d.Task.Size
	d.Task.SingleConn = true
	d.Task.ResumeSupport = false
	if d.Task.Error == "" {
		d.Task.Error = "restart required with one connection: " + cause.Error()
	}
	d.Task.mu.Unlock()
	if size > 0 {
		d.resetSegments(size)
	}
	d.Stop()
}

// assemble trims the sparse part file to its exact size and moves it into place.
func (d *Downloader) assemble() error {
	t := d.Task
	t.mu.Lock()
	t.Status = StatusAssembling
	t.mu.Unlock()

	// Publish the final segment table before the task can be observed as
	// complete, so a finished row always carries its full segment map.
	d.syncTask()

	if d.file != nil {
		t.mu.Lock()
		size := t.Size
		t.mu.Unlock()
		if size > 0 {
			if err := d.file.Truncate(size); err != nil {
				return err
			}
		} else if st, err := d.file.Stat(); err == nil {
			t.mu.Lock()
			t.Size = st.Size()
			size = st.Size()
			t.mu.Unlock()
		}
		_ = d.file.Sync()
		_ = d.file.Close()
		d.file = nil
	}
	if err := os.MkdirAll(t.Dir, 0o755); err != nil {
		return err
	}
	dst := uniquePath(filepath.Join(t.Dir, t.Filename))
	if err := moveFile(t.TempPath, dst); err != nil {
		// Matches the observed "Access Denied error while assembling file parts".
		return fmt.Errorf("cannot assemble the file at %s: %w (it may be open in another application)", dst, err)
	}
	t.mu.Lock()
	t.Filename = filepath.Base(dst)
	t.MarkComplete()
	t.mu.Unlock()
	_ = os.Remove(t.MetaPath)
	d.logf("assembled %s", dst)
	return nil
}

func uniquePath(p string) string {
	if _, err := os.Stat(p); err != nil {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 1; i < 1000; i++ {
		cand := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(cand); err != nil {
			return cand
		}
	}
	return p
}

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		in.Close()
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		in.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		in.Close()
		return err
	}
	// The source handle must be released before removing it, or Windows reports
	// a sharing violation ("used by another process") for a cross-volume move.
	if err := in.Close(); err != nil {
		return err
	}
	return os.Remove(src)
}

func mapToHeader(m map[string]string) http.Header {
	h := http.Header{}
	for k, v := range m {
		h.Set(k, v)
	}
	return h
}
