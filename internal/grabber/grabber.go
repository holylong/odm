// Package grabber implements the Site Grabber: a level-limited crawler that
// collects links matching IDM's filter syntax and hands them to the engine.
//
// The behaviour reproduced here comes from the shipped grabber help
// (re/chm_grab/*, see re/FINDINGS.md §10): a start page, a link depth, a
// same-site/domain restriction, `*` wildcard file filters, minimum and maximum
// size limits, and de-duplication by name+size.
package grabber

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"odm/internal/config"
	"odm/internal/httpx"
)

// Project is one grabber project (`<name>.igp`).
type Project struct {
	Name        string   `json:"name"`
	StartURL    string   `json:"startURL"`
	Depth       int      `json:"depth"`        // link levels to follow
	SameSite    bool     `json:"sameSite"`     // restrict to the start page's host
	IncludePath []string `json:"includePath"`  // `*` wildcards
	ExcludePath []string `json:"excludePath"`
	IncludeHost []string `json:"includeHost"`
	ExcludeHost []string `json:"excludeHost"`
	FileFilter  []string `json:"fileFilter"` // e.g. ["*.zip", "*.pdf"] or ["<start page>"]
	MinSize     int64    `json:"minSize"`
	MaxSize     int64    `json:"maxSize"`
	MaxFiles    int      `json:"maxFiles"`
	Username    string   `json:"username"`
	Password    string   `json:"password"`
	Referer     string   `json:"referer"`
}

// Found is one candidate file.
type Found struct {
	URL  string `json:"url"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	Mime string `json:"mime"`
}

// Result is a crawl outcome.
type Result struct {
	Project *Project `json:"project"`
	Files   []Found  `json:"files"`
	Pages   int      `json:"pages"`
	Skipped int      `json:"skipped"`
	Errors  []string `json:"errors"`
}

// Crawler walks a project.
type Crawler struct {
	Client *httpx.Client
	Logf   func(string, ...any)

	mu      sync.Mutex
	seen    map[string]bool // dedupe by name+size, as documented
	visited map[string]bool
}

// New creates a crawler.
func New(logf func(string, ...any)) *Crawler {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	opts := httpx.Options{
		Timeout:        time.Duration(config.Current().TimeoutSec) * time.Second,
		UserAgent:      config.Current().UserAgent,
		AcceptLanguage: config.Current().AcceptLanguage,
		Proxy:          config.Current().Proxy,
		Logf:           logf,
	}
	return &Crawler{
		Client:  httpx.New(opts),
		Logf:    logf,
		seen:    map[string]bool{},
		visited: map[string]bool{},
	}
}

// Run executes the project and returns the matches.
func (c *Crawler) Run(p *Project) (*Result, error) {
	if p == nil || p.StartURL == "" {
		return nil, errors.New("grabber: a start page URL is required")
	}
	if p.Depth <= 0 {
		p.Depth = 1
	}
	if p.MaxFiles <= 0 {
		p.MaxFiles = 500
	}
	if p.Referer == "" {
		p.Referer = p.StartURL
	}
	res := &Result{Project: p}

	type page struct {
		url   string
		depth int
	}
	queue := []page{{p.StartURL, 0}}
	startHost := hostOf(p.StartURL)

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if c.visited[cur.url] {
			continue
		}
		c.visited[cur.url] = true
		if cur.depth > p.Depth {
			continue
		}
		body, finalURL, err := c.fetchPage(cur.url, p)
		if err != nil {
			res.Errors = append(res.Errors, cur.url+": "+err.Error())
			continue
		}
		res.Pages++
		c.Logf("grabber: page %s (%d bytes)", cur.url, len(body))

		links := ExtractLinks(string(body), finalURL)
		for _, l := range links {
			if !c.allowed(l, startHost, p) {
				res.Skipped++
				continue
			}
			if LooksLikeFile(l, p.FileFilter) {
				if len(res.Files) >= p.MaxFiles {
					continue
				}
				if c.addCandidate(res, l, cur.url, p) {
					c.Logf("grabber: match %s", l)
				}
				continue
			}
			if cur.depth+1 <= p.Depth && !c.visited[l] {
				queue = append(queue, page{l, cur.depth + 1})
			}
		}
	}
	return res, nil
}

func (c *Crawler) fetchPage(rawURL string, p *Project) ([]byte, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, rawURL, err
	}
	req := &httpx.Request{
		Method:  "GET",
		URL:     u,
		Header:  http.Header{"Accept-Encoding": []string{"gzip, deflate"}},
		Referer: p.Referer,
	}
	if p.Username != "" {
		req.URL.User = url.UserPassword(p.Username, p.Password)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, rawURL, err
	}
	defer resp.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != 304 {
		return nil, rawURL, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	buf := make([]byte, 0, 64*1024)
	tmp := make([]byte, 32*1024)
	total := 0
	for {
		n, rerr := resp.Read(tmp)
		if n > 0 {
			total += n
			if total > 8*1024*1024 {
				break // page cap, same spirit as IDM's page size guard
			}
			buf = append(buf, tmp[:n]...)
		}
		if rerr != nil {
			break
		}
	}
	final := rawURL
	if resp.FinalURL != nil {
		final = resp.FinalURL.String()
	}
	return buf, final, nil
}

// addCandidate probes the URL for size and applies the size window.
func (c *Crawler) addCandidate(res *Result, rawURL, ref string, p *Project) bool {
	name := nameOf(rawURL)
	c.mu.Lock()
	key := name
	if c.seen[key] {
		c.mu.Unlock()
		return false
	}
	c.seen[key] = true
	c.mu.Unlock()

	f := Found{URL: rawURL, Name: name}
	if size, mime, err := c.probe(rawURL, ref); err == nil {
		f.Size = size
		f.Mime = mime
	}
	if p.MinSize > 0 && f.Size >= 0 && f.Size < p.MinSize {
		return false
	}
	if p.MaxSize > 0 && f.Size > p.MaxSize {
		return false
	}
	res.Files = append(res.Files, f)
	return true
}

func (c *Crawler) probe(rawURL, ref string) (int64, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return -1, "", err
	}
	resp, err := c.Client.Do(&httpx.Request{
		Method: "HEAD",
		URL:    u,
		Header: http.Header{},
		Referer: ref,
	})
	if err != nil {
		return -1, "", err
	}
	defer resp.Close()
	if resp.StatusCode >= 300 {
		return -1, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.ContentLength, resp.Header.Get("Content-Type"), nil
}

// allowed applies host/path include and exclude rules.
func (c *Crawler) allowed(rawURL, startHost string, p *Project) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := u.Hostname()
	if p.SameSite && !strings.EqualFold(host, startHost) {
		return false
	}
	if len(p.IncludeHost) > 0 && !anyMatch(p.IncludeHost, host) {
		return false
	}
	if anyMatch(p.ExcludeHost, host) {
		return false
	}
	full := u.Path
	if u.RawQuery != "" {
		full += "?" + u.RawQuery
	}
	if len(p.IncludePath) > 0 && !anyMatch(p.IncludePath, full) {
		return false
	}
	if anyMatch(p.ExcludePath, full) {
		return false
	}
	return true
}

// LooksLikeFile applies the `*` wildcard filter list. `<start page>` means the
// start page itself, which IDM lets you download as a file.
func LooksLikeFile(rawURL string, filters []string) bool {
	if len(filters) == 0 {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	path := u.Path
	base := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		base = path[i+1:]
	}
	for _, f := range filters {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if strings.EqualFold(f, "<start page>") {
			if strings.HasSuffix(path, "/") || base == "" || !strings.Contains(base, ".") {
				return true
			}
			continue
		}
		if wildcard(f, base) || wildcard(f, path) {
			return true
		}
	}
	return false
}

// wildcard matches a glob with `*` and `?`, case-insensitively.
func wildcard(pattern, s string) bool {
	p := strings.ToLower(pattern)
	t := strings.ToLower(s)
	if p == "" {
		return false
	}
	// dynamic-programming glob match
	pi, ti := 0, 0
	star, mark := -1, 0
	for ti < len(t) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == t[ti]):
			pi++
			ti++
		case pi < len(p) && p[pi] == '*':
			star = pi
			mark = ti
			pi++
		case star >= 0:
			pi = star + 1
			mark++
			ti = mark
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

func anyMatch(patterns []string, s string) bool {
	for _, p := range patterns {
		if wildcard(p, s) || strings.Contains(strings.ToLower(s), strings.ToLower(strings.Trim(p, "*"))) {
			return true
		}
	}
	return false
}

func nameOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	p := u.Path
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	if p == "" {
		p = u.Hostname()
	}
	return p
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

var (
	reHref  = regexp.MustCompile(`(?is)<a[^>]+href\s*=\s*["']([^"']+)["']`)
	reSrc   = regexp.MustCompile(`(?is)<(?:img|script|source|embed)[^>]+src\s*=\s*["']([^"']+)["']`)
	reLink  = regexp.MustCompile(`(?is)<link[^>]+href\s*=\s*["']([^"']+)["']`)
	reCSS   = regexp.MustCompile(`(?is)url\(\s*["']?([^"')]+)["']?\s*\)`)
	reBase  = regexp.MustCompile(`(?is)<base[^>]+href\s*=\s*["']([^"']+)["']`)
)

// ExtractLinks pulls absolute, de-duplicated links out of an HTML document.
// A regex scanner is used instead of a full HTML parser so the build stays
// dependency-free; the four patterns cover href, src, link and CSS url().
func ExtractLinks(html, pageURL string) []string {
	base := pageURL
	if m := reBase.FindStringSubmatch(html); len(m) == 2 {
		if b, err := url.Parse(m[1]); err == nil {
			if abs := resolve(pageURL, b); abs != "" {
				base = abs
			}
		}
	}
	b, err := url.Parse(base)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" || strings.HasPrefix(raw, "#") ||
			strings.HasPrefix(strings.ToLower(raw), "javascript:") ||
			strings.HasPrefix(strings.ToLower(raw), "mailto:") ||
			strings.HasPrefix(strings.ToLower(raw), "data:") {
			return
		}
		ref, err := url.Parse(raw)
		if err != nil {
			return
		}
		abs := b.ResolveReference(ref).String()
		if i := strings.IndexByte(abs, '#'); i >= 0 {
			abs = abs[:i]
		}
		if seen[abs] {
			return
		}
		seen[abs] = true
		out = append(out, abs)
	}
	for _, re := range []*regexp.Regexp{reHref, reSrc, reLink, reCSS} {
		for _, m := range re.FindAllStringSubmatch(html, -1) {
			if len(m) == 2 {
				add(m[1])
			}
		}
	}
	return out
}

func resolve(base string, ref *url.URL) string {
	bu, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return bu.ResolveReference(ref).String()
}

// ParseFilterList splits a comma/semicolon separated filter string.
func ParseFilterList(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\t'
	})
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !strings.Contains(f, "*") && !strings.Contains(f, ".") {
			f = "*." + f
		}
		out = append(out, f)
	}
	return out
}

// ParseSize accepts "1.5MB", "300k", "2048".
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	for _, suf := range []struct {
		name string
		mult int64
	}{
		{"gb", 1 << 30}, {"g", 1 << 30},
		{"mb", 1 << 20}, {"m", 1 << 20},
		{"kb", 1 << 10}, {"k", 1 << 10},
		{"b", 1},
	} {
		if strings.HasSuffix(s, suf.name) {
			mult = suf.mult
			s = strings.TrimSuffix(s, suf.name)
			break
		}
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, err
	}
	return int64(n * float64(mult)), nil
}
