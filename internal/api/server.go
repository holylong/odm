// Package api serves the ODM desktop UI and the local JSON API that the browser
// extension talks to.
//
// Rationale: IDMan.exe is a Win32 GUI plus a native-messaging host registered as
// com.tonec.idm (re/FINDINGS.md §7). Rebuilding the Win32 shell buys nothing that
// can be verified here, so the GUI is a loopback web app and the browser bridge
// speaks the same two transports the shipped extension uses: a native messaging
// host process and a loopback HTTP/WebSocket channel.
package api

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"odm/internal/config"
	"odm/internal/engine"
	"odm/internal/grabber"
	"odm/internal/httpx"
)

//go:embed web
var webFS embed.FS

// Server wires the manager to HTTP.
type Server struct {
	Mgr  *engine.Manager
	Logf func(string, ...any)
	Port int

	logs   *ringLog
	srv    *http.Server
	started time.Time
}

// New builds a server around a manager.
func New(mgr *engine.Manager, port int, logf func(string, ...any)) *Server {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Server{Mgr: mgr, Logf: logf, Port: port, logs: newRingLog(500), started: time.Now()}
}

// Addr is the loopback base URL.
func (s *Server) Addr() string { return fmt.Sprintf("http://127.0.0.1:%d", s.Port) }

// Handler builds the mux (exported for tests).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/logs", s.handleLogs)
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("POST /api/settings", s.handleSetSettings)
	mux.HandleFunc("POST /api/tasks", s.handleAddTask)
	mux.HandleFunc("POST /api/tasks/{id}/pause", s.handlePause)
	mux.HandleFunc("POST /api/tasks/{id}/resume", s.handleResume)
	mux.HandleFunc("POST /api/tasks/{id}/remove", s.handleRemove)
	mux.HandleFunc("GET /api/tasks/{id}", s.handleTaskDetail)
	mux.HandleFunc("POST /api/grabber/run", s.handleGrabber)
	mux.HandleFunc("POST /api/bridge/takeover", s.handleTakeover)
	mux.HandleFunc("GET /api/bridge/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "version": "1.0", "port": s.Port})
	})

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return logRequests(mux, s.logs)
}

// Listen binds the loopback port and serves until Close.
func (s *Server) Listen() error {
	if s.Port == 0 {
		s.Port = config.Current().BridgePort
	}
	var l net.Listener
	var err error
	for attempt := 0; attempt < 12; attempt++ {
		l, err = net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(s.Port)))
		if err == nil {
			break
		}
		s.Port++
	}
	if err != nil {
		return err
	}
	s.srv = &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = s.srv.Serve(l) }()
	return nil
}

// Close stops the server.
func (s *Server) Close() error {
	if s.srv == nil {
		return nil
	}
	return s.srv.Close()
}

// ------------------------------------------------------------------- handlers

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	tasks := engine.SortByStatus(s.Mgr.List())
	out := make([]json.RawMessage, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, marshalTask(t))
	}
	writeJSON(w, 200, map[string]any{
		"tasks":      out,
		"settings":   config.Current(),
		"totalSpeed": s.Mgr.TotalSpeed(),
		"port":       s.Port,
		"uptimeSec":  int(time.Since(s.started).Seconds()),
		"categories": categories(tasks),
	})
}

func categories(tasks []*engine.Task) []string {
	set := map[string]bool{"General": true}
	for _, t := range tasks {
		if t.Category != "" {
			set[t.Category] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

func marshalTask(t *engine.Task) json.RawMessage {
	t.LockMarshal()
	defer t.UnlockMarshal()
	b, err := json.Marshal(t)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

func (s *Server) handleTaskDetail(w http.ResponseWriter, r *http.Request) {
	t := s.Mgr.Get(r.PathValue("id"))
	if t == nil {
		writeJSON(w, 404, map[string]any{"error": "no such task"})
		return
	}
	writeJSON(w, 200, marshalTask(t))
}

func (s *Server) handleAddTask(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL      string            `json:"url"`
		Dir      string            `json:"dir"`
		Filename string            `json:"filename"`
		Referer  string            `json:"referer"`
		QueueID  string            `json:"queueID"`
		Category string            `json:"category"`
		StartNow *bool             `json:"startNow"`
		Headers  map[string]string `json:"headers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	if strings.TrimSpace(body.URL) == "" {
		writeJSON(w, 400, map[string]any{"error": "url is required"})
		return
	}
	startNow := true
	if body.StartNow != nil {
		startNow = *body.StartNow
	}
	t, err := s.Mgr.Add(engine.AddOptions{
		URL: body.URL, Dir: body.Dir, Filename: body.Filename, Referer: body.Referer,
		QueueID: body.QueueID, Category: body.Category, Headers: body.Headers, StartNow: startNow,
	})
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, marshalTask(t))
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	s.Mgr.Pause(r.PathValue("id"))
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Mgr.Resume(id); err != nil {
		writeJSON(w, 404, map[string]any{"error": "no such task"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleRemove(w http.ResponseWriter, r *http.Request) {
	del := r.URL.Query().Get("file") == "1"
	s.Mgr.Remove(r.PathValue("id"), del)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, config.Current())
}

func (s *Server) handleSetSettings(w http.ResponseWriter, r *http.Request) {
	cur := *config.Current()
	if err := json.NewDecoder(r.Body).Decode(&cur); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	config.Set(&cur)
	if err := config.Save(); err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	s.Logf("settings saved to %s", config.Path())
	writeJSON(w, 200, config.Current())
}

func (s *Server) handleLogs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, s.logs.Slice())
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ch := s.Mgr.Subscribe()
	defer s.Mgr.Unsubscribe(ch)

	// initial snapshot as one event so the client can render immediately
	enc := json.NewEncoder(w)
	send := func(name string, v any) bool {
		if _, err := fmt.Fprintf(w, "event: %s\ndata: ", name); err != nil {
			return false
		}
		if err := enc.Encode(v); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	tasks := engine.SortByStatus(s.Mgr.List())
	snap := make([]json.RawMessage, 0, len(tasks))
	for _, t := range tasks {
		snap = append(snap, marshalTask(t))
	}
	if !send("snapshot", map[string]any{"tasks": snap, "totalSpeed": s.Mgr.TotalSpeed()}) {
		return
	}

	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		case ev, ok := <-ch:
			if !ok {
				return
			}
			switch ev.Type {
			case "log":
				if !send("log", map[string]any{"line": ev.Line}) {
					return
				}
			default:
				payload := map[string]any{"type": ev.Type, "taskID": ev.TaskID}
				if ev.Task != nil {
					payload["task"] = marshalTask(ev.Task)
				}
				if !send("task", payload) {
					return
				}
			}
		}
	}
}

func (s *Server) handleGrabber(w http.ResponseWriter, r *http.Request) {
	var proj grabber.Project
	if err := json.NewDecoder(r.Body).Decode(&proj); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	c := grabber.New(func(f string, a ...any) { s.Logf(f, a...) })
	res, err := c.Run(&proj)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, res)
}

// handleTakeover is the browser bridge decision point: given what the browser
// knows about a navigation, decide whether ODM should take the download over.
// This mirrors the File Types tab semantics observed in the binary
// (extension list, excluded sites, excluded addresses) — re/FINDINGS.md §4/§10.
func (s *Server) handleTakeover(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL       string `json:"url"`
		Referer   string `json:"referer"`
		Filename  string `json:"filename"`
		Mime      string `json:"mime"`
		Size      int64  `json:"size"`
		UserAgent string `json:"userAgent"`
		TabID     int    `json:"tabID"`
		Silent    bool   `json:"silent"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	sets := config.Current()
	ext := grabberExt(body.Filename, body.URL)

	takeover := false
	reason := ""
	switch {
	case body.URL == "":
		reason = "no url"
	case matchesAny(sets.FileTypes.ExcludedSites, body.URL):
		reason = "excluded site"
	case matchesAny(sets.FileTypes.ExcludedAddress, hostOf(body.URL)):
		reason = "excluded address"
	case strings.HasPrefix(strings.ToLower(body.Mime), "text/html"):
		reason = "html page"
	case ext == "":
		reason = "no file extension"
	case !containsFold(sets.FileTypes.AutoStart, ext):
		reason = "extension " + ext + " is not in the file types list"
	default:
		takeover = true
		reason = "extension " + ext + " is in the file types list"
	}

	var id string
	if takeover {
		name := body.Filename
		if name == "" {
			name = engine.FilenameFromURL(body.URL)
		}
		t, err := s.Mgr.Add(engine.AddOptions{
			URL: body.URL, Filename: name, Referer: body.Referer,
			StartNow: true, Category: categoryFor(ext),
		})
		if err == nil {
			id = t.ID
		}
	}
	s.Logf("bridge: %s -> takeover=%v (%s)", body.URL, takeover, reason)
	writeJSON(w, 200, map[string]any{
		"takeover": takeover,
		"reason":   reason,
		"taskID":   id,
		"ext":      ext,
	})
}

func categoryFor(ext string) string {
	switch ext {
	case "mp4", "mkv", "avi", "mov", "flv", "webm", "wmv", "m4v", "mpg", "mpeg":
		return "Video"
	case "mp3", "m4a", "wav", "ogg", "aac", "wma", "flac":
		return "Music"
	case "pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "txt", "epub":
		return "Documents"
	case "exe", "msi", "msp", "apk", "deb", "dmg":
		return "Programs"
	case "zip", "rar", "7z", "gz", "tar", "iso", "bz2", "xz":
		return "Compressed"
	}
	return "General"
}

func grabberExt(filename, rawURL string) string {
	if filename != "" {
		if i := strings.LastIndexByte(filename, '.'); i >= 0 && i < len(filename)-1 {
			return strings.ToLower(filename[i+1:])
		}
	}
	name := engine.FilenameFromURL(rawURL)
	if i := strings.LastIndexByte(name, '.'); i >= 0 && i < len(name)-1 {
		return strings.ToLower(name[i+1:])
	}
	return ""
}

func matchesAny(patterns []string, s string) bool {
	low := strings.ToLower(s)
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if strings.Contains(low, strings.Trim(p, "*")) {
			return true
		}
	}
	return false
}

func containsFold(list []string, v string) bool {
	for _, s := range list {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}

func hostOf(rawURL string) string {
	i := strings.Index(rawURL, "://")
	if i < 0 {
		return rawURL
	}
	rest := rawURL[i+3:]
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		rest = rest[:j]
	}
	if j := strings.IndexByte(rest, ':'); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// ------------------------------------------------------------------- plumbing

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ringLog keeps the last N log lines for the UI's log pane.
type ringLog struct {
	mu   sync.Mutex
	buf  []logLine
	max  int
}

type logLine struct {
	At   time.Time `json:"at"`
	Line string    `json:"line"`
}

func newRingLog(n int) *ringLog { return &ringLog{max: n} }

func (r *ringLog) Add(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, logLine{At: time.Now(), Line: s})
	if len(r.buf) > r.max {
		r.buf = r.buf[len(r.buf)-r.max:]
	}
}

func (r *ringLog) Slice() []logLine {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]logLine, len(r.buf))
	copy(out, r.buf)
	return out
}

func logRequests(next http.Handler, logs *ringLog) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || strings.HasPrefix(r.URL.Path, "/api/") {
			logs.Add(r.Method + " " + r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

// LogfRecord logs and stores, used by cmd for startup lines.
func (s *Server) LogfRecord(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	s.Logf("%s", line)
	s.logs.Add(line)
}

var _ = os.Stat
var _ = filepath.Join
var _ = httpx.ErrNoResume
