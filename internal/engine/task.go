package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"odm/internal/httpx"
)

// Status is a download state.
type Status string

// States, named after the IDM UI states in the shipped help.
const (
	StatusQueued      Status = "queued"
	StatusProbing     Status = "probing"
	StatusDownloading Status = "downloading"
	StatusPaused      Status = "paused"
	StatusAssembling  Status = "assembling"
	StatusComplete    Status = "complete"
	StatusError       Status = "error"
)

// Segment is one byte range of a task. End is inclusive; Pos is the next byte
// still to be fetched. This is the unit that the in-half division rule splits.
type Segment struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
	Pos   int64 `json:"pos"`
	Done  bool  `json:"done"`
	Claim int   `json:"claim"`
}

// Remaining reports bytes left inside the segment.
func (s *Segment) Remaining() int64 {
	if s.Done || s.Pos > s.End {
		return 0
	}
	return s.End - s.Pos + 1
}

// Task is one download.
type Task struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	FinalURL string `json:"finalURL"`
	Filename string `json:"filename"`
	Dir      string `json:"dir"`
	TempPath string `json:"tempPath"`
	MetaPath string `json:"metaPath"`

	Size       int64 `json:"size"` // -1 while unknown
	Downloaded int64 `json:"downloaded"`
	Status     Status `json:"status"`
	Error      string `json:"error"`

	Segments      []Segment `json:"segments"`
	ResumeSupport bool      `json:"resumeSupport"`
	SingleConn    bool      `json:"singleConn"`
	Connections   int       `json:"connections"`

	Referer string            `json:"referer"`
	Cookies []httpx.Cookie    `json:"cookies"`
	Headers map[string]string `json:"headers"`
	Mime    string            `json:"mime"`

	QueueID  string `json:"queueID"`
	Category string `json:"category"`

	CreatedAt   time.Time `json:"createdAt"`
	CompletedAt time.Time `json:"completedAt"`
	LastModTime string    `json:"lastModTime"`

	// live, not persisted
	Speed    float64 `json:"-"`
	mu       sync.Mutex
	cancel   chan struct{}
	lastTick int64
	lastBytes int64
}

// Meta is the on-disk resume record (`<id>.odm`), the analogue of IDM saving file
// positions several times per minute (re/FINDINGS.md §8).
type Meta struct {
	ID            string            `json:"id"`
	URL           string            `json:"url"`
	FinalURL      string            `json:"finalURL"`
	Filename      string            `json:"filename"`
	Dir           string            `json:"dir"`
	Size          int64             `json:"size"`
	Segments      []Segment         `json:"segments"`
	ResumeSupport bool              `json:"resumeSupport"`
	SingleConn    bool              `json:"singleConn"`
	Connections   int               `json:"connections"`
	Referer       string            `json:"referer"`
	Cookies       []httpx.Cookie    `json:"cookies"`
	Headers       map[string]string `json:"headers"`
	Mime          string            `json:"mime"`
	LastModTime   string            `json:"lastModTime"`
	QueueID       string            `json:"queueID"`
	Category      string            `json:"category"`
	CreatedAt     time.Time         `json:"createdAt"`
}

// NewTask builds a task with paths allocated under tempDir/parts.
func NewTask(id, rawURL, dir, filename, tempDir string) *Task {
	parts := filepath.Join(tempDir, "parts")
	return &Task{
		ID:       id,
		URL:      rawURL,
		Dir:      dir,
		Filename: filename,
		TempPath: filepath.Join(parts, id+".part"),
		MetaPath: filepath.Join(parts, id+".odm"),
		Size:     -1,
		Status:   StatusQueued,
		Headers:  map[string]string{},
		CreatedAt: time.Now(),
	}
}

// TotalSegments returns how many splits have happened (for the UI's pink lines).
func (t *Task) SegmentStarts() []int64 {
	out := make([]int64, 0, len(t.Segments))
	for _, s := range t.Segments {
		out = append(out, s.Start)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Progress is the 0..1 fraction complete.
func (t *Task) Progress() float64 {
	if t.Size <= 0 {
		if t.Status == StatusComplete {
			return 1
		}
		return 0
	}
	p := float64(t.RecomputeDownloaded()) / float64(t.Size)
	if p > 1 {
		p = 1
	}
	return p
}

// LockMarshal / UnlockMarshal let the API snapshot a task consistently.
func (t *Task) LockMarshal()   { t.mu.Lock() }
func (t *Task) UnlockMarshal() { t.mu.Unlock() }

// Snapshot marshals a consistent view of the task.
func (t *Task) Snapshot() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	b, err := json.Marshal(t)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// RecomputeDownloaded sums finished bytes from the segment table.
func (t *Task) RecomputeDownloaded() int64 {
	var n int64
	for i := range t.Segments {
		n += t.Segments[i].Pos - t.Segments[i].Start
	}
	return n
}

// Save writes the resume record atomically. A completed task has nothing to
// resume, so any stale record of it is removed instead of rewritten.
func (t *Task) Save() error {
	if t.MetaPath == "" {
		return nil
	}
	if t.Status == StatusComplete {
		if err := os.Remove(t.MetaPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	m := Meta{
		ID: t.ID, URL: t.URL, FinalURL: t.FinalURL, Filename: t.Filename, Dir: t.Dir,
		Size: t.Size, Segments: t.Segments, ResumeSupport: t.ResumeSupport,
		SingleConn: t.SingleConn, Connections: t.Connections, Referer: t.Referer,
		Cookies: t.Cookies, Headers: t.Headers, Mime: t.Mime, LastModTime: t.LastModTime,
		QueueID: t.QueueID, Category: t.Category, CreatedAt: t.CreatedAt,
	}
	b, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(t.MetaPath), 0o755); err != nil {
		return err
	}
	tmp := t.MetaPath + ".new"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, t.MetaPath)
}

// LoadMeta reads a resume record.
func LoadMeta(path string) (*Meta, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// TaskFromMeta rebuilds a paused Task from disk.
func TaskFromMeta(m *Meta, tempDir string) *Task {
	parts := filepath.Join(tempDir, "parts")
	t := &Task{
		ID: m.ID, URL: m.URL, FinalURL: m.FinalURL, Filename: m.Filename, Dir: m.Dir,
		TempPath: filepath.Join(parts, m.ID+".part"),
		MetaPath: filepath.Join(parts, m.ID+".odm"),
		Size:     m.Size, Segments: m.Segments, ResumeSupport: m.ResumeSupport,
		SingleConn: m.SingleConn, Connections: m.Connections, Referer: m.Referer,
		Cookies: m.Cookies, Headers: m.Headers, Mime: m.Mime, LastModTime: m.LastModTime,
		QueueID: m.QueueID, Category: m.Category, CreatedAt: m.CreatedAt,
		Status:  StatusPaused,
	}
	if t.Headers == nil {
		t.Headers = map[string]string{}
	}
	t.Downloaded = t.RecomputeDownloaded()
	return t
}

// MarkComplete flips all segments to done.
func (t *Task) MarkComplete() {
	for i := range t.Segments {
		t.Segments[i].Pos = t.Segments[i].End + 1
		t.Segments[i].Done = true
	}
	t.Status = StatusComplete
	t.CompletedAt = time.Now()
}

// ---------------------------------------------------------------- segmentation

// Segments is the segment table with the in-half division rule.
type Segments struct {
	list []Segment
}

// NewSegments creates the initial single segment covering the whole file.
func NewSegments(size int64) *Segments {
	return &Segments{list: []Segment{{Start: 0, End: size - 1, Pos: 0}}}
}

// Wrap adopts an existing table (after resume).
func Wrap(s []Segment) *Segments { return &Segments{list: s} }

// List returns the table.
func (s *Segments) List() []Segment { return s.list }

// SplitLargest halves the segment with the most remaining bytes and returns the
// index of the new (second) half plus the index of the segment it came from.
// This is IDM's documented rule:
// "When new connection becomes available IDM finds the largest segment to
// download and divide it in half." (re/FINDINGS.md §8, segmentation.chm)
//
// New halves are appended rather than inserted: workers address their segment by
// index, so existing indexes must never shift underneath a running transfer. The
// UI sorts by Start when it draws the segment bar.
//
// It refuses to split a segment whose remaining bytes are below minSize.
func (s *Segments) SplitLargest(minSize int64) (newIdx, srcIdx int, ok bool) {
	best := -1
	var bestRem int64
	for i := range s.list {
		rem := s.list[i].Remaining()
		if rem <= 0 || s.list[i].Done {
			continue
		}
		if rem > bestRem {
			bestRem = rem
			best = i
		}
	}
	if best < 0 || bestRem < minSize*2 {
		return 0, 0, false
	}
	seg := &s.list[best]
	rem := seg.Remaining()
	mid := seg.Pos + rem/2
	newSeg := Segment{Start: mid, End: seg.End, Pos: mid}
	seg.End = mid - 1
	s.list = append(s.list, newSeg)
	return len(s.list) - 1, best, true
}

// ClaimFree claims an unclaimed, unfinished segment.
func (s *Segments) ClaimFree(worker int) (int, bool) {
	for i := range s.list {
		if s.list[i].Done || s.list[i].Claim != 0 {
			continue
		}
		if s.list[i].Remaining() <= 0 {
			continue
		}
		s.list[i].Claim = worker
		return i, true
	}
	return 0, false
}

// Release frees a claim.
func (s *Segments) Release(idx, worker int) {
	if idx >= 0 && idx < len(s.list) && s.list[idx].Claim == worker {
		s.list[idx].Claim = 0
	}
}

// AllDone reports whether every segment finished.
func (s *Segments) AllDone() bool {
	for i := range s.list {
		if !s.list[i].Done {
			return false
		}
	}
	return len(s.list) > 0
}

// Reset collapses back to a single segment from zero, used by the
// "start over with one connection" degrade path (re/FINDINGS.md §6).
func (s *Segments) Reset(size int64) {
	s.list = []Segment{{Start: 0, End: size - 1, Pos: 0}}
}

// ---------------------------------------------------------------- url helpers

// SanitizeFilename strips characters Windows rejects.
func SanitizeFilename(s string) string {
	// Keep only the final path segment: a server-supplied name must never be able
	// to walk out of the save directory.
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	bad := `\/:*?"<>|` + "\r\n\t"
	out := strings.Map(func(r rune) rune {
		if strings.ContainsRune(bad, r) {
			return '_'
		}
		return r
	}, s)
	out = strings.Trim(out, " .")
	if isDeviceName(out) {
		out = "_" + out
	}
	return out
}

// isDeviceName reports the reserved Windows device names that cannot be used as
// a file name on this platform.
func isDeviceName(s string) bool {
	if s == "" {
		return false
	}
	base := s
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	switch strings.ToUpper(base) {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return true
	}
	return false
}

// FilenameFromURL derives a default file name.
func FilenameFromURL(rawURL string) string {
	u := rawURL
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	u = strings.TrimRight(u, "/")
	if i := strings.LastIndexAny(u, "/\\"); i >= 0 {
		u = u[i+1:]
	}
	if u == "" || strings.Contains(u, ":") {
		return "download"
	}
	if dec, err := decodePath(u); err == nil && dec != "" {
		u = dec
	}
	return SanitizeFilename(u)
}

func decodePath(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			n, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
			if err != nil {
				b.WriteByte(s[i])
				continue
			}
			b.WriteByte(byte(n))
			i += 2
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String(), nil
}

// Ext returns the lowercase extension without the dot.
func Ext(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i < 0 || i == len(name)-1 {
		return ""
	}
	return strings.ToLower(name[i+1:])
}
