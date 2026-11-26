package grabber

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
)

func TestWildcard(t *testing.T) {
	cases := []struct {
		pat, s string
		want   bool
	}{
		{"*.zip", "https://x/a.zip", true},
		{"*.zip", "https://x/a.rar", false},
		{"*.tonec.com", "download.tonec.com", true},
		{"*.tonec.com", "tonec.com", false},
		{"a*c", "abc", true},
		{"a*c", "abx", false},
		{"*", "anything", true},
		{"?b", "ab", true},
		{"?b", "abc", false},
	}
	for _, c := range cases {
		if got := wildcard(c.pat, c.s); got != c.want {
			t.Errorf("wildcard(%q,%q) = %v, want %v", c.pat, c.s, got, c.want)
		}
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"10":      10,
		"10k":     10 * 1024,
		"10K":     10 * 1024,
		"2m":      2 * 1024 * 1024,
		"1g":      1024 * 1024 * 1024,
		"1.5k":    1536,
		"":        0,
		" 4 kb ":  4 * 1024,
	}
	for in, want := range cases {
		got, err := ParseSize(in)
		if err != nil {
			t.Fatalf("ParseSize(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("ParseSize(%q) = %d, want %d", in, got, want)
		}
	}
	if _, err := ParseSize("abc"); err == nil {
		t.Error("ParseSize(\"abc\") should fail")
	}
}

func TestLooksLikeFile(t *testing.T) {
	// With no filter list the grabber has no policy and matches nothing.
	if LooksLikeFile("https://x/setup.exe", nil) {
		t.Error("an empty filter list must not match")
	}
	if !LooksLikeFile("https://x/setup.exe", []string{"*"}) {
		t.Error("* should match any path")
	}
	if !LooksLikeFile("https://x/setup.exe", []string{"*.exe"}) {
		t.Error("*.exe should match setup.exe")
	}
	if LooksLikeFile("https://x/index.html", []string{"*.zip", "*.pdf"}) {
		t.Error("an html page is not a file for these filters")
	}
	if !LooksLikeFile("https://x/page", []string{"<start page>"}) {
		t.Error("<start page> should accept a page without an extension")
	}
	if !LooksLikeFile("https://x/", []string{"<start page>"}) {
		t.Error("<start page> should accept a directory URL")
	}
	if LooksLikeFile("https://x/a.rar", []string{"*.zip"}) {
		t.Error("*.zip filter must reject a .rar")
	}
}

func TestExtractLinks(t *testing.T) {
	html := `<html><body>
	  <a href="/files/a.zip">a</a>
	  <a href='b.rar'>b</a>
	  <img src="pic.png">
	  <a href="https://other/x.pdf">c</a>
	  <script src="app.js"></script>
	  <a href="#anchor">skip</a>
	  <a href="mailto:x@y.z">skip</a>
	</body></html>`
	links := ExtractLinks(html, "https://site.test/dir/page.html")
	set := map[string]bool{}
	for _, l := range links {
		set[l] = true
	}
	for _, want := range []string{
		"https://site.test/files/a.zip",
		"https://site.test/dir/b.rar",
		"https://site.test/dir/pic.png",
		"https://other/x.pdf",
		"https://site.test/dir/app.js",
	} {
		if !set[want] {
			t.Errorf("missing link %s; got %v", want, links)
		}
	}
	for _, bad := range []string{"#anchor", "mailto:x@y.z"} {
		for _, l := range links {
			if l == bad {
				t.Errorf("should not include %q", bad)
			}
		}
	}
}

// TestCrawlerRun crawls a two-level test site and collects the zip/pdf files.
func TestCrawlerRun(t *testing.T) {
	var mux *http.ServeMux
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r)
	}))
	defer srv.Close()

	pages := map[string]string{
		"/": `<html><body>
			<a href="/files/one.zip">one</a>
			<a href="/files/two.pdf">two</a>
			<a href="/deep.html">deep</a>
			<a href="/files/notes.html">notes</a>
		</body></html>`,
		"/deep.html": `<html><body>
			<a href="/files/three.rar">three</a>
			<a href="/files/one.zip">dup</a>
		</body></html>`,
		"/files/notes.html": `<html><body>not a file</body></html>`,
	}
	files := map[string][]byte{
		"/files/one.zip":   make([]byte, 2048),
		"/files/two.pdf":   make([]byte, 4096),
		"/files/three.rar": make([]byte, 8192),
	}
	mux = http.NewServeMux()
	for p, body := range pages {
		b := []byte(body)
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Length", itoa(len(b)))
			_, _ = w.Write(b)
		})
	}
	for p, body := range files {
		b := body
		mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", itoa(len(b)))
			if r.Method == "HEAD" {
				return
			}
			_, _ = w.Write(b)
		})
	}

	c := New(nil)
	defer c.Client.Close()

	p := &Project{
		Name:       "test",
		StartURL:   srv.URL + "/",
		Depth:      2,
		SameSite:   true,
		FileFilter: []string{"*.zip", "*.pdf", "*.rar"},
	}
	res, err := c.Run(p)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range res.Files {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	want := []string{"one.zip", "three.rar", "two.pdf"}
	if len(names) != len(want) {
		t.Fatalf("found %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("found %v, want %v", names, want)
		}
	}
	// Sizes come from the HEAD/GET probe, and the duplicate one.zip is deduped.
	for _, f := range res.Files {
		switch f.Name {
		case "one.zip":
			if f.Size != 2048 {
				t.Errorf("one.zip size = %d, want 2048", f.Size)
			}
		case "three.rar":
			if f.Size != 8192 {
				t.Errorf("three.rar size = %d, want 8192", f.Size)
			}
		}
	}
	if res.Pages < 2 {
		t.Errorf("crawled %d pages, want at least 2", res.Pages)
	}
	t.Logf("crawled %d pages, found %v", res.Pages, names)
}

// TestCrawlerSizeFilter proves Min/Max size filtering works on the wire.
func TestCrawlerSizeFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			body := []byte(`<a href="/small.zip">s</a><a href="/big.zip">b</a>`)
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Content-Length", itoa(len(body)))
			_, _ = w.Write(body)
			return
		}
		size := 1024
		if r.URL.Path == "/big.zip" {
			size = 100 * 1024
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Length", itoa(size))
		if r.Method != "HEAD" {
			_, _ = w.Write(make([]byte, size))
		}
	}))
	defer srv.Close()

	c := New(nil)
	defer c.Client.Close()
	res, err := c.Run(&Project{
		StartURL:   srv.URL + "/",
		Depth:      1,
		SameSite:   true,
		FileFilter: []string{"*.zip"},
		MinSize:    50 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 1 || res.Files[0].Name != "big.zip" {
		t.Fatalf("size filter kept %v, want only big.zip", res.Files)
	}
	t.Logf("size filter kept %s (%d bytes), skipped %d", res.Files[0].Name, res.Files[0].Size, res.Skipped)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
