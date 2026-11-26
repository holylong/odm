package engine

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"odm/internal/config"
	"odm/internal/httpx"
)

// Event is broadcast to UI subscribers.
type Event struct {
	Type   string `json:"type"` // added|updated|removed|log
	TaskID string `json:"taskID,omitempty"`
	Task   *Task  `json:"task,omitempty"`
	Line   string `json:"line,omitempty"`
}

// Manager owns every task, the scheduler queue and the running downloaders.
type Manager struct {
	mu      sync.Mutex
	tasks   map[string]*Task
	order   []string
	running map[string]*Downloader
	sem     chan struct{}
	subs    map[chan Event]struct{}
	logf    func(string, ...any)
	dir     string
	started time.Time
}

// NewManager loads persisted tasks from tempDir.
func NewManager(tempDir string, logf func(string, ...any)) *Manager {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	m := &Manager{
		tasks:   map[string]*Task{},
		running: map[string]*Downloader{},
		sem:     make(chan struct{}, config.Current().MaxConcurrentDownloads),
		subs:    map[chan Event]struct{}{},
		logf:    logf,
		dir:     tempDir,
		started: time.Now(),
	}
	m.load()
	go m.speedTicker()
	return m
}

// Subscribe registers an event listener.
func (m *Manager) Subscribe() chan Event {
	ch := make(chan Event, 64)
	m.mu.Lock()
	m.subs[ch] = struct{}{}
	m.mu.Unlock()
	return ch
}

// Unsubscribe drops a listener.
func (m *Manager) Unsubscribe(ch chan Event) {
	m.mu.Lock()
	delete(m.subs, ch)
	m.mu.Unlock()
}

func (m *Manager) emit(e Event) {
	m.mu.Lock()
	subs := make([]chan Event, 0, len(m.subs))
	for c := range m.subs {
		subs = append(subs, c)
	}
	m.mu.Unlock()
	for _, c := range subs {
		select {
		case c <- e:
		default:
		}
	}
}

// Log publishes a log line to subscribers and the process logger.
func (m *Manager) Log(format string, args ...any) {
	m.logf(format, args...)
	m.emit(Event{Type: "log", Line: format})
}

func (m *Manager) logfFor(id string) func(string, ...any) {
	return func(format string, args ...any) {
		m.logf("["+shortID(id)+"] "+format, args...)
	}
}

func shortID(id string) string {
	if len(id) > 6 {
		return id[:6]
	}
	return id
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ---------------------------------------------------------------- persistence

func (m *Manager) storePath() string { return filepath.Join(m.dir, "tasks.json") }

// storingTask is the persisted shape (no live fields).
type storingTask struct {
	*Task
}

func (m *Manager) save() {
	m.mu.Lock()
	list := make([]*Task, 0, len(m.order))
	for _, id := range m.order {
		if t, ok := m.tasks[id]; ok {
			list = append(list, t)
		}
	}
	m.mu.Unlock()

	out := make([]json.RawMessage, 0, len(list))
	for _, t := range list {
		t.mu.Lock()
		b, err := json.Marshal(t)
		t.mu.Unlock()
		if err == nil {
			out = append(out, b)
		}
	}
	blob, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		return
	}
	tmp := m.storePath() + ".new"
	if err := os.WriteFile(tmp, blob, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, m.storePath())
}

func (m *Manager) load() {
	b, err := os.ReadFile(m.storePath())
	if err != nil {
		return
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range raw {
		var t Task
		if err := json.Unmarshal(r, &t); err != nil {
			continue
		}
		if t.Status == StatusDownloading || t.Status == StatusProbing || t.Status == StatusAssembling || t.Status == StatusQueued {
			t.Status = StatusPaused
		}
		if t.Headers == nil {
			t.Headers = map[string]string{}
		}
		if t.MetaPath == "" {
			t.MetaPath = filepath.Join(m.dir, "parts", t.ID+".odm")
		}
		if t.TempPath == "" {
			t.TempPath = filepath.Join(m.dir, "parts", t.ID+".part")
		}
		d := t.Downloaded
		_ = d
		m.tasks[t.ID] = &t
		m.order = append(m.order, t.ID)
	}
}

// ---------------------------------------------------------------- task API

// AddOptions carries the Download File Info dialog fields.
type AddOptions struct {
	URL      string
	Dir      string
	Filename string
	Referer  string
	Cookies  []httpx.Cookie
	Headers  map[string]string
	QueueID  string
	Category string
	// StartNow=false behaves like IDM's /a: queue without starting.
	StartNow bool
	Silent   bool
}

// Add registers a new download and optionally starts it.
func (m *Manager) Add(o AddOptions) (*Task, error) {
	s := config.Current()
	if o.Dir == "" {
		o.Dir = s.DefaultDir
	}
	if o.Headers == nil {
		o.Headers = map[string]string{}
	}
	t := NewTask(newID(), o.URL, o.Dir, SanitizeFilename(o.Filename), s.TempDir)
	t.Referer = o.Referer
	t.Cookies = o.Cookies
	t.Headers = o.Headers
	t.QueueID = o.QueueID
	t.Category = o.Category
	if t.Category == "" {
		t.Category = "General"
	}

	m.mu.Lock()
	m.tasks[t.ID] = t
	m.order = append(m.order, t.ID)
	m.mu.Unlock()

	m.emit(Event{Type: "added", TaskID: t.ID, Task: t})
	m.save()
	if o.StartNow {
		go m.start(t.ID)
	}
	return t, nil
}

// Resume restarts a paused or failed task.
func (m *Manager) Resume(id string) error {
	m.mu.Lock()
	t, ok := m.tasks[id]
	m.mu.Unlock()
	if !ok {
		return os.ErrNotExist
	}
	t.mu.Lock()
	if t.Status == StatusComplete || t.Status == StatusDownloading {
		t.mu.Unlock()
		return nil
	}
	t.Error = ""
	t.Status = StatusQueued
	t.mu.Unlock()
	go m.start(id)
	return nil
}

// Pause stops a running task, keeping the resume record.
func (m *Manager) Pause(id string) {
	m.mu.Lock()
	d := m.running[id]
	t := m.tasks[id]
	m.mu.Unlock()
	if d != nil {
		d.Stop()
	}
	if t != nil {
		t.mu.Lock()
		if t.Status == StatusDownloading || t.Status == StatusProbing || t.Status == StatusQueued {
			t.Status = StatusPaused
		}
		t.mu.Unlock()
		m.emit(Event{Type: "updated", TaskID: id, Task: t})
	}
}

// Remove deletes a task, its resume record and its part file.
func (m *Manager) Remove(id string, deleteFile bool) {
	m.Pause(id)
	m.mu.Lock()
	t, ok := m.tasks[id]
	if ok {
		delete(m.tasks, id)
		for i, v := range m.order {
			if v == id {
				m.order = append(m.order[:i], m.order[i+1:]...)
				break
			}
		}
	}
	m.mu.Unlock()
	if ok {
		_ = os.Remove(t.MetaPath)
		_ = os.Remove(t.TempPath)
		if deleteFile {
			_ = os.Remove(filepath.Join(t.Dir, t.Filename))
		}
		m.emit(Event{Type: "removed", TaskID: id})
	}
	m.save()
}

// Get returns a task by id.
func (m *Manager) Get(id string) *Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tasks[id]
}

// List returns tasks in insertion order.
func (m *Manager) List() []*Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Task, 0, len(m.order))
	for _, id := range m.order {
		if t, ok := m.tasks[id]; ok {
			out = append(out, t)
		}
	}
	return out
}

// ---------------------------------------------------------------- scheduler

// start runs one task through the concurrency semaphore.
func (m *Manager) start(id string) {
	t := m.Get(id)
	if t == nil {
		return
	}
	m.sem <- struct{}{}
	defer func() { <-m.sem }()

	m.mu.Lock()
	if _, dup := m.running[id]; dup {
		m.mu.Unlock()
		return
	}
	t.mu.Lock()
	if t.Status == StatusComplete {
		t.mu.Unlock()
		m.mu.Unlock()
		return
	}
	t.Status = StatusQueued
	cookies := append([]httpx.Cookie(nil), t.Cookies...)
	referer := t.Referer
	t.mu.Unlock()

	opts := httpx.Options{
		Timeout:        time.Duration(config.Current().TimeoutSec) * time.Second,
		UserAgent:      config.Current().UserAgent,
		AcceptLanguage: config.Current().AcceptLanguage,
		Proxy:          config.Current().Proxy,
		Logf:           m.logfFor(id),
	}
	client := httpx.New(opts)
	if len(cookies) > 0 {
		host := hostOf(t.URL)
		client.SetCookies(host, cookies)
	}
	_ = referer

	d := NewDownloader(t, client, m.logfFor(id))
	m.running[id] = d
	m.mu.Unlock()

	m.emit(Event{Type: "updated", TaskID: id, Task: t})
	err := d.Run()

	m.mu.Lock()
	delete(m.running, id)
	m.mu.Unlock()
	client.Close()

	t.mu.Lock()
	switch {
	case err == nil:
		t.Error = ""
	case err == ErrStopped:
		if t.Status != StatusPaused {
			t.Status = StatusPaused
		}
		t.Error = ""
	default:
		t.Status = StatusError
		t.Error = err.Error()
		m.logf("[%s] error: %v", shortID(id), err)
	}
	if t.SingleConn && t.Status == StatusError && t.Error != "" {
		// Leave a resumable task paused after the single-connection degrade so
		// the user can press Resume, which is what IDM asks for.
		t.Status = StatusPaused
	}
	t.mu.Unlock()
	m.emit(Event{Type: "updated", TaskID: id, Task: t})
	m.save()
}

func hostOf(rawURL string) string {
	for i := 0; i+2 < len(rawURL); i++ {
		if rawURL[i:i+3] == "://" {
			rest := rawURL[i+3:]
			for j := 0; j < len(rest); j++ {
				if rest[j] == '/' || rest[j] == ':' || rest[j] == '?' {
					return rest[:j]
				}
			}
			return rest
		}
	}
	return ""
}

// speedTicker samples per-task byte counters once a second.
func (m *Manager) speedTicker() {
	type sample struct {
		bytes int64
		at    time.Time
	}
	prev := map[string]sample{}
	tk := time.NewTicker(time.Second)
	defer tk.Stop()
	for range tk.C {
		now := time.Now()
		for _, t := range m.List() {
			t.mu.Lock()
			cur := t.RecomputeDownloaded()
			old, ok := prev[t.ID]
			if ok && now.Sub(old.at) > 0 {
				dt := now.Sub(old.at).Seconds()
				sp := float64(cur-old.bytes) / dt
				if sp < 0 {
					sp = 0
				}
				t.Speed = sp
			}
			prev[t.ID] = sample{bytes: cur, at: now}
			if t.Status != StatusDownloading {
				t.Speed = 0
			}
			t.mu.Unlock()
		}
	}
}

// TotalSpeed sums the live speed of every task.
func (m *Manager) TotalSpeed() float64 {
	var s float64
	for _, t := range m.List() {
		t.mu.Lock()
		s += t.Speed
		t.mu.Unlock()
	}
	return s
}

// ResumePending restarts everything that was interrupted, used by `odm /s`.
func (m *Manager) ResumePending() int {
	n := 0
	for _, t := range m.List() {
		t.mu.Lock()
		needStart := t.Status == StatusPaused || t.Status == StatusQueued
		queued := t.QueueID
		t.mu.Unlock()
		if needStart && queued != "" {
			_ = m.Resume(t.ID)
			n++
		}
	}
	return n
}

// SortByStatus returns tasks ordered the way the IDM list shows them.
func SortByStatus(ts []*Task) []*Task {
	out := append([]*Task(nil), ts...)
	rank := func(s Status) int {
		switch s {
		case StatusDownloading, StatusProbing, StatusAssembling:
			return 0
		case StatusQueued:
			return 1
		case StatusPaused:
			return 2
		case StatusError:
			return 3
		default:
			return 4
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank(out[i].Status), rank(out[j].Status)
		if ri != rj {
			return ri < rj
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}
