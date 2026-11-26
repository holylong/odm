package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"odm/internal/config"
	"odm/internal/engine"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	s := config.Default()
	s.DefaultDir = dir
	s.TempDir = filepath.Join(dir, "temp")
	s.MinSegmentSize = 64 * 1024
	config.Set(s)
	mgr := engine.NewManager(s.TempDir, nil)
	return New(mgr, 0, func(string, ...any) {})
}

func call(t *testing.T, h http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	out := map[string]any{}
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

// TestTakeoverPolicy checks the File Types policy that drives the browser
// extension: an archive is taken over, an html page is not, an excluded site is
// not, and an unknown extension defers to the browser.
func TestTakeoverPolicy(t *testing.T) {
	s := testServer(t)
	h := s.Handler()

	cases := []struct {
		name  string
		url   string
		files string
		mime  string
		want  bool
		why   string
	}{
		{"zip", "https://example.com/files/a.zip", "a.zip", "", true, "in the file types list"},
		{"exe", "https://example.com/setup.exe", "", "application/octet-stream", true, "in the file types list"},
		{"html", "https://example.com/index.html", "index.html", "text/html; charset=utf-8", false, "html page"},
		{"unknown ext", "https://example.com/page", "", "text/plain", false, "no file extension"},
		{"not listed", "https://example.com/a.xyz", "a.xyz", "", false, "not in the file types list"},
		{"excluded site", "https://download.windowsupdate.com/a.zip", "a.zip", "", false, "excluded site"},
	}
	for _, c := range cases {
		code, body := call(t, h, "POST", "/api/bridge/takeover", map[string]any{
			"url": c.url, "filename": c.files, "mime": c.mime, "referer": "https://example.com/",
		})
		if code != 200 {
			t.Fatalf("%s: status %d", c.name, code)
		}
		got, _ := body["takeover"].(bool)
		reason, _ := body["reason"].(string)
		if got != c.want {
			t.Fatalf("%s: takeover=%v (%s), want %v — reason %q", c.name, got, reason, c.want, reason)
		}
		if !strings.Contains(strings.ToLower(reason), strings.ToLower(c.why)) {
			t.Errorf("%s: reason %q should mention %q", c.name, reason, c.why)
		}
	}
}

// TestStateShape pins the JSON the UI and the extension rely on.
func TestStateShape(t *testing.T) {
	s := testServer(t)
	h := s.Handler()

	code, body := call(t, h, "GET", "/api/state", nil)
	if code != 200 {
		t.Fatalf("state: %d", code)
	}
	for _, key := range []string{"tasks", "settings", "totalSpeed", "port", "uptimeSec", "categories"} {
		if _, ok := body[key]; !ok {
			t.Errorf("/api/state is missing %q", key)
		}
	}
	if _, ok := body["tasks"].([]any); !ok {
		t.Errorf("tasks must be an array, got %T", body["tasks"])
	}
	settings, ok := body["settings"].(map[string]any)
	if !ok {
		t.Fatal("settings must be an object")
	}
	// The JSON names mirror the registry value names observed in IDMan.exe.
	for _, key := range []string{"MaxConnectionsNumber", "MinSegmentSize", "FileTypes", "UserAgent"} {
		if _, ok := settings[key]; !ok {
			t.Errorf("settings is missing %q", key)
		}
	}
}

func TestPingAndStaticUI(t *testing.T) {
	s := testServer(t)
	h := s.Handler()

	code, body := call(t, h, "GET", "/api/bridge/ping", nil)
	if code != 200 || body["ok"] != true {
		t.Fatalf("ping = %d %v", code, body)
	}

	for _, path := range []string{"/", "/app.js", "/style.css"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		if rec.Body.Len() < 100 {
			t.Fatalf("%s: only %d bytes", path, rec.Body.Len())
		}
	}
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(strings.ToLower(rec.Body.String()), "odm") {
		t.Error("index.html should mention ODM")
	}
}

func TestAddPauseRemoveTask(t *testing.T) {
	s := testServer(t)

	// A URL that refuses connections: the task is created and then errors, which
	// is enough to exercise add/list/detail/remove without a live server.
	code, body := call(t, s.Handler(), "POST", "/api/tasks", map[string]any{
		"url": "http://127.0.0.1:1/nothing.zip", "filename": "nothing.zip",
	})
	if code != 200 {
		t.Fatalf("add: %d %v", code, body)
	}
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("add returned no id: %v", body)
	}

	code, body = call(t, s.Handler(), "GET", "/api/tasks/"+id, nil)
	if code != 200 {
		t.Fatalf("detail: %d", code)
	}
	if body["id"] != id {
		t.Errorf("detail id = %v, want %s", body["id"], id)
	}

	code, _ = call(t, s.Handler(), "GET", "/api/tasks/nope", nil)
	if code != 404 {
		t.Errorf("missing task should be 404, got %d", code)
	}

	code, _ = call(t, s.Handler(), "POST", "/api/tasks/"+id+"/remove", map[string]any{"deleteFile": true})
	if code != 200 {
		t.Fatalf("remove: %d", code)
	}
	if s.Mgr.Get(id) != nil {
		t.Error("task still listed after remove")
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	s := testServer(t)
	_, body := call(t, s.Handler(), "GET", "/api/settings", nil)
	if body["MaxConnectionsNumber"] == nil {
		t.Fatalf("settings missing: %v", body)
	}

	body["MaxConnectionsNumber"] = 5.0
	code, _ := call(t, s.Handler(), "POST", "/api/settings", body)
	if code != 200 {
		t.Fatalf("save settings: %d", code)
	}
	if got := config.Current().MaxConnectionsNumber; got != 5 {
		t.Fatalf("MaxConnectionsNumber = %d, want 5", got)
	}
}

func TestMain(m *testing.M) {
	// Keep the real user settings file out of reach.
	dir, err := os.MkdirTemp("", "odm-api-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("ODM_CONFIG", filepath.Join(dir, "odm.json"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
