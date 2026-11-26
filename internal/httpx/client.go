// Package httpx is a hand-written HTTP/1.1 client.
//
// Why not net/http: IDMan.exe emits its own request line and header block, which
// is visible verbatim in the shipped binary (re/FINDINGS.md §2). Byte-range
// correctness depends on details net/http hides — header order, an identity
// Accept-Encoding on segmented requests, Cookie2, and connection reuse across
// successive range requests on the same socket. This package reproduces that
// wire format and keeps the socket for the next segment.
package httpx

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"odm/internal/config"
)

// ErrNoResume reports that the server refused a ranged request, matching IDM's
// "server does not support resume" condition (re/FINDINGS.md §5).
var ErrNoResume = errors.New("server does not support resume")

// Range is a byte range. A nil *Range means "no Range header".
type Range struct {
	Start  int64
	End    int64
	HasEnd bool
}

// String renders the range using the exact shape observed in the binary:
//   Range: bytes=0-%d | bytes=0- | bytes=%d- | bytes=%d-%d
func (r *Range) String() string {
	if r == nil {
		return ""
	}
	if r.HasEnd {
		return fmt.Sprintf("bytes=%d-%d", r.Start, r.End)
	}
	return fmt.Sprintf("bytes=%d-", r.Start)
}

// ContentRange is a parsed Content-Range response header.
type ContentRange struct {
	Start, End, Total int64
	HasTotal          bool
}

// Options configures a Client.
type Options struct {
	Timeout        time.Duration
	UserAgent      string
	AcceptLanguage string
	Proxy          config.Proxy
	InsecureTLS    bool
	MaxIdlePerHost int
	// Logf receives protocol-level diagnostics; nil disables logging.
	Logf func(format string, args ...any)
}

// Cookie is a name/value pair sent with a request.
type Cookie struct {
	Name  string
	Value string
}

// Request describes one request.
type Request struct {
	Method           string
	URL              *url.URL
	Range            *Range
	Header           http.Header
	Referer          string
	Cookies          []Cookie
	IdentityEncoding bool
	// Body is an optional request body (used by the grabber's POST search).
	Body        []byte
	ContentType string
}

// Response is a parsed response with a streaming body.
type Response struct {
	StatusCode    int
	Reason        string
	Proto         string
	Header        http.Header
	ContentLength int64 // -1 when unknown
	ContentRange  *ContentRange
	AcceptRanges  bool
	Filename      string
	FinalURL      *url.URL
	Request       *Request

	br       *bufio.Reader
	cn       *conn
	client   *Client
	remain   int64 // -1 => until EOF
	chunked  bool
	eof      bool
	noBody   bool
	drainErr error
}

// Client is a keep-alive-capable HTTP client with no net/http machinery.
type Client struct {
	Opts Options

	mu    sync.Mutex
	idle  map[string][]*conn
	jc    map[string]string // cookie jar: "host|name" -> value
	dials int
}

// New creates a Client.
func New(opts Options) *Client {
	if opts.Timeout <= 0 {
		opts.Timeout = 30 * time.Second
	}
	if opts.MaxIdlePerHost <= 0 {
		opts.MaxIdlePerHost = 8
	}
	return &Client{Opts: opts, idle: map[string][]*conn{}, jc: map[string]string{}}
}

func (c *Client) logf(format string, args ...any) {
	if c.Opts.Logf != nil {
		c.Opts.Logf(format, args...)
	}
}

// SetCookies records cookies for a host so later segment requests reuse them,
// as IDM does ("Full reuse of connections ... without additional connect and
// login stages", re/FINDINGS.md §8).
func (c *Client) SetCookies(host string, cookies []Cookie) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ck := range cookies {
		c.jc[host+"|"+ck.Name] = ck.Value
	}
}

// CookiesFor returns stored cookies for a host.
func (c *Client) CookiesFor(host string) []Cookie {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Cookie
	prefix := host + "|"
	for k, v := range c.jc {
		if strings.HasPrefix(k, prefix) {
			out = append(out, Cookie{Name: k[len(prefix):], Value: v})
		}
	}
	return out
}

// Close releases every idle socket.
func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, list := range c.idle {
		for _, cn := range list {
			c.closeConn(cn, "client closed")
		}
	}
	c.idle = map[string][]*conn{}
}

// closeConn shuts a socket down. With ODM_TRACE_CLOSE set it records the reason
// and the stack that closed it, which is how a stray close during a ranged read
// gets located.
func (c *Client) closeConn(cn *conn, why string) {
	if cn == nil || cn.c == nil {
		return
	}
	if os.Getenv("ODM_TRACE_CLOSE") != "" {
		c.logf("CLOSE %s: %s\n%s", cn.addr, why, string(debug.Stack()))
	}
	_ = cn.c.Close()
}

// conn is one TCP/TLS socket plus its read buffer.
type conn struct {
	c      net.Conn
	br     *bufio.Reader
	scheme string
	addr   string
	reuse  int
	pooled bool
}

// release returns a drained socket to the idle pool so the next segment can skip
// TCP/TLS setup, mirroring IDM's connection reuse (re/FINDINGS.md §8).
func (c *Client) release(cn *conn) {
	if cn == nil || cn.c == nil {
		return
	}
	key := "tcp|" + cn.addr
	c.mu.Lock()
	list := c.idle[key]
	if len(list) >= c.Opts.MaxIdlePerHost {
		c.mu.Unlock()
		c.closeConn(cn, "idle pool full")
		return
	}
	c.idle[key] = append(list, cn)
	c.mu.Unlock()
	_ = cn.c.SetDeadline(time.Time{})
}

// Do sends a request and returns the response. Redirects are followed up to 10
// hops, preserving Method for 307/308 and switching to GET for 301/302/303.
func (c *Client) Do(req *Request) (*Response, error) {
	cur := *req
	if cur.Method == "" {
		cur.Method = "GET"
	}
	if cur.URL == nil {
		return nil, errors.New("httpx: nil URL")
	}
	for hop := 0; hop <= 10; hop++ {
		resp, err := c.do1(&cur)
		if err != nil {
			return nil, err
		}
		switch resp.StatusCode {
		case 301, 302, 303, 307, 308:
			loc := resp.Header.Get("Location")
			if loc == "" {
				return resp, nil
			}
			resp.drain()
			next, err := cur.URL.Parse(loc)
			if err != nil {
				return nil, err
			}
			c.logf("redirect %d -> %s", resp.StatusCode, next)
			cur.URL = next
			if resp.StatusCode == 303 || ((resp.StatusCode == 301 || resp.StatusCode == 302) && cur.Method != "GET" && cur.Method != "HEAD") {
				cur.Method = "GET"
				cur.Body = nil
			}
			continue
		default:
			return resp, nil
		}
	}
	return nil, errors.New("httpx: too many redirects")
}

func (c *Client) do1(req *Request) (*Response, error) {
	isTLS := req.URL.Scheme == "https"
	host := req.URL.Hostname()
	port := req.URL.Port()
	if port == "" {
		if isTLS {
			port = "443"
		} else {
			port = "80"
		}
	}
	addr := net.JoinHostPort(host, port)

	cn, err := c.acquire(addr, isTLS, host)
	if err != nil {
		return nil, err
	}

	if err := c.writeRequest(cn, req); err != nil {
		c.closeConn(cn, "write failed")
		return nil, err
	}
	resp, err := readResponse(cn.br, req)
	if err != nil {
		c.closeConn(cn, "read response failed")
		return nil, err
	}
	resp.cn = cn
	resp.client = c
	resp.Request = req
	resp.FinalURL = req.URL

	// 401/407 challenge handling (Basic and Digest), per the observed
	// Authorization/Proxy-Authorization templates.
	if resp.StatusCode == 401 || resp.StatusCode == 407 {
		hdrName := "WWW-Authenticate"
		if resp.StatusCode == 407 {
			hdrName = "Proxy-Authenticate"
		}
		ch := resp.Header.Get(hdrName)
		if ch != "" {
			resp.drain()
			authUser, authPass := c.credsFor(req.URL, resp.StatusCode)
			if authUser != "" {
				val := BuildAuthorization(ch, req.Method, req.URL.RequestURI(), authUser, authPass, c.nextNonce())
				if val != "" {
					if resp.StatusCode == 407 {
						req.Header.Set("Proxy-Authorization", val)
					} else {
						req.Header.Set("Authorization", val)
					}
					return c.do1(req)
				}
			}
		}
	}
	return resp, nil
}

// credsFor picks credentials for a challenge: per-URL login list first, then the
// proxy credentials observed as Http/Https/Ftp user/password.
func (c *Client) credsFor(u *url.URL, status int) (string, string) {
	for _, l := range config.Current().Logins {
		if l.URL != "" && strings.HasPrefix(u.String(), l.URL) {
			return l.UserName, l.Password
		}
	}
	p := c.Opts.Proxy
	if status == 407 {
		if p.UseHttpsProxy && p.HttpsUserName != "" {
			return p.HttpsUserName, p.HttpsPasword
		}
		if p.HttpUserName != "" {
			return p.HttpUserName, p.HttpPasword
		}
		return "", ""
	}
	if u.User != nil {
		pw, _ := u.User.Password()
		return u.User.Username(), pw
	}
	return "", ""
}

var nonceSeq int
var nonceMu sync.Mutex

func (c *Client) nextNonce() string {
	nonceMu.Lock()
	nonceSeq++
	n := nonceSeq
	nonceMu.Unlock()
	return fmt.Sprintf("%08x%08x", n, time.Now().UnixNano()&0xffffffff)
}

func (c *Client) acquire(addr string, isTLS bool, host string) (*conn, error) {
	key := "tcp|" + addr
	c.mu.Lock()
	if list := c.idle[key]; len(list) > 0 {
		cn := list[len(list)-1]
		c.idle[key] = list[:len(list)-1]
		c.mu.Unlock()
		cn.reuse++
		c.logf("reuse connection %s (#%d)", addr, cn.reuse)
		return cn, nil
	}
	c.dials++
	c.mu.Unlock()

	c.logf("connect %s", addr)
	raw, err := c.dialTarget(addr, host)
	if err != nil {
		return nil, err
	}
	if tc, ok := raw.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	if isTLS {
		tc := tls.Client(raw, &tls.Config{
			ServerName:         host,
			InsecureSkipVerify: c.Opts.InsecureTLS,
			NextProtos:         []string{"http/1.1"},
			MinVersion:         tls.VersionTLS12,
		})
		_ = tc.SetDeadline(time.Now().Add(c.Opts.Timeout))
		if err := tc.Handshake(); err != nil {
			raw.Close()
			return nil, fmt.Errorf("tls handshake with %s: %w", host, err)
		}
		_ = tc.SetDeadline(time.Time{})
		raw = tc
	}
	_ = raw.SetDeadline(time.Now().Add(c.Opts.Timeout))
	scheme := "http"
	if isTLS {
		scheme = "https"
	}
	return &conn{c: raw, br: bufio.NewReaderSize(raw, 64*1024), scheme: scheme, addr: addr}, nil
}

// dialTarget connects to addr, going through SOCKS or an HTTP proxy when set.
func (c *Client) dialTarget(addr, host string) (net.Conn, error) {
	p := c.Opts.Proxy
	d := &net.Dialer{Timeout: c.Opts.Timeout, KeepAlive: 30 * time.Second}

	if p.Enable && p.UseSocks && p.SocksHost != "" {
		socksAddr := net.JoinHostPort(p.SocksHost, itoa(p.SocksPort))
		raw, err := d.Dial("tcp", socksAddr)
		if err != nil {
			return nil, fmt.Errorf("cannot connect to the socks server %s: %w", p.SocksHost, err)
		}
		_ = raw.SetDeadline(time.Now().Add(c.Opts.Timeout))
		if err := socksHandshake(raw, p, host, addr, c.Opts.Timeout); err != nil {
			raw.Close()
			return nil, err
		}
		_ = raw.SetDeadline(time.Time{})
		return raw, nil
	}

	proxyHost, proxyPort, useProxy := c.proxyFor(host)
	if !useProxy {
		return d.Dial("tcp", addr)
	}
	proxyAddr := net.JoinHostPort(proxyHost, itoa(proxyPort))
	raw, err := d.Dial("tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to proxy server %s:%d: %w", proxyHost, proxyPort, err)
	}
	// HTTP proxies only need CONNECT for TLS; plain HTTP uses absolute-URI form.
	// We always CONNECT so the request line stays origin-form like IDM's.
	_ = raw.SetDeadline(time.Now().Add(c.Opts.Timeout))
	if err := c.proxyConnect(raw, proxyAddr, addr); err != nil {
		raw.Close()
		return nil, err
	}
	_ = raw.SetDeadline(time.Time{})
	return raw, nil
}

func (c *Client) proxyFor(host string) (string, int, bool) {
	p := c.Opts.Proxy
	if !p.Enable {
		return "", 0, false
	}
	switch {
	case p.UseHttpsProxy && p.HttpsProxy != "":
		port := p.HttpsPort
		if port == 0 {
			port = 8080
		}
		return p.HttpsProxy, port, true
	case p.UseHttpProxy && p.HttpProxy != "":
		port := p.HttpPort
		if port == 0 {
			port = 8080
		}
		return p.HttpProxy, port, true
	}
	return "", 0, false
}

// proxyConnect performs the CONNECT handshake using the observed template.
func (c *Client) proxyConnect(raw net.Conn, proxyAddr, target string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "CONNECT %s HTTP/1.1\r\n", target)
	fmt.Fprintf(&b, "Host: %s\r\n", target)
	if c.Opts.UserAgent != "" {
		fmt.Fprintf(&b, "User-Agent: %s\r\n", c.Opts.UserAgent)
	}
	p := c.Opts.Proxy
	user := p.HttpUserName
	pass := p.HttpPasword
	if p.UseHttpsProxy && p.HttpsUserName != "" {
		user, pass = p.HttpsUserName, p.HttpsPasword
	}
	if user != "" {
		basic := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		fmt.Fprintf(&b, "Proxy-Authorization: Basic %s\r\n", basic)
	}
	b.WriteString("Proxy-Connection: Keep-Alive\r\n\r\n")
	if _, err := io.WriteString(raw, b.String()); err != nil {
		return err
	}
	br := bufio.NewReader(raw)
	line, err := br.ReadString('\n')
	if err != nil {
		return err
	}
	code := parseStatusCode(line)
	if code != 200 {
		return fmt.Errorf("cannot connect to proxy server %s (CONNECT returned %d)", proxyAddr, code)
	}
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		if l == "\r\n" || l == "\n" {
			break
		}
	}
	if br.Buffered() > 0 {
		// Proxy sent bytes immediately after CONNECT (rare); keep them.
		buf := make([]byte, br.Buffered())
		_, _ = io.ReadFull(br, buf)
		if len(buf) > 0 {
			return fmt.Errorf("unexpected %d bytes after proxy CONNECT", len(buf))
		}
	}
	return nil
}

// writeRequest renders the request using the templates observed in IDMan.exe.
func (c *Client) writeRequest(cn *conn, req *Request) error {
	target := req.URL.RequestURI()
	if target == "" {
		target = "/"
	}
	hostHeader := req.URL.Host
	if p := req.URL.Port(); p == "" || (cn.scheme == "http" && p == "80") || (cn.scheme == "https" && p == "443") {
		hostHeader = req.URL.Hostname()
	}
	if req.URL.User != nil {
		hostHeader = req.URL.Host
	}

	var b strings.Builder
	// "%s %s HTTP/1.1"
	fmt.Fprintf(&b, "%s %s HTTP/1.1\r\n", req.Method, target)
	fmt.Fprintf(&b, "Host: %s\r\n", hostHeader)
	ua := req.Header.Get("User-Agent")
	if ua == "" {
		ua = c.Opts.UserAgent
	}
	if ua != "" {
		fmt.Fprintf(&b, "User-Agent: %s\r\n", ua)
	}
	accept := req.Header.Get("Accept")
	if accept == "" {
		accept = "*/*"
	}
	fmt.Fprintf(&b, "Accept: %s\r\n", accept)
	b.WriteString("Connection: Keep-Alive\r\n")
	b.WriteString("Cache-Control: no-cache\r\n")
	b.WriteString("Pragma: no-cache\r\n")

	if enc := req.Header.Get("Accept-Encoding"); enc != "" {
		fmt.Fprintf(&b, "Accept-Encoding: %s\r\n", enc)
	} else if req.IdentityEncoding {
		// Segmented transfers must stay identity or byte offsets shift.
		b.WriteString("Accept-Encoding: identity\r\n")
	}
	if al := req.Header.Get("Accept-Language"); al != "" {
		fmt.Fprintf(&b, "Accept-Language: %s\r\n", al)
	} else if c.Opts.AcceptLanguage != "" {
		fmt.Fprintf(&b, "Accept-Language: %s\r\n", c.Opts.AcceptLanguage)
	}
	if ch := req.Header.Get("Accept-Charset"); ch != "" {
		fmt.Fprintf(&b, "Accept-Charset: %s\r\n", ch)
	}
	if o := req.Header.Get("Origin"); o != "" {
		fmt.Fprintf(&b, "Origin: %s\r\n", o)
	}
	if req.Referer != "" {
		fmt.Fprintf(&b, "Referer: %s\r\n", req.Referer)
	}
	if ims := req.Header.Get("If-Modified-Since"); ims != "" {
		fmt.Fprintf(&b, "If-Modified-Since: %s\r\n", ims)
	}
	if rng := req.Range.String(); rng != "" {
		fmt.Fprintf(&b, "Range: %s\r\n", rng)
	}
	if req.Body != nil {
		ct := req.ContentType
		if ct == "" {
			ct = "application/x-www-form-urlencoded"
		}
		fmt.Fprintf(&b, "Content-Type: %s\r\n", ct)
		fmt.Fprintf(&b, "Content-Length: %d\r\n", len(req.Body))
	}

	// Cookies: stored jar + per-request list, de-duplicated by name.
	seen := map[string]bool{}
	var pairs []string
	for _, ck := range c.CookiesFor(req.URL.Hostname()) {
		if !seen[ck.Name] {
			seen[ck.Name] = true
			pairs = append(pairs, ck.Name+"="+ck.Value)
		}
	}
	for _, ck := range req.Cookies {
		if !seen[ck.Name] {
			seen[ck.Name] = true
			pairs = append(pairs, ck.Name+"="+ck.Value)
		}
	}
	if len(pairs) > 0 {
		fmt.Fprintf(&b, "Cookie: %s\r\n", strings.Join(pairs, "; "))
	}

	// Any remaining caller headers (Authorization, custom X-*).
	for k, vs := range req.Header {
		switch strings.ToLower(k) {
		case "user-agent", "accept", "accept-encoding", "accept-language",
			"accept-charset", "origin", "referer", "if-modified-since",
			"content-type", "content-length", "host", "connection",
			"cache-control", "pragma", "range", "cookie":
			continue
		}
		for _, v := range vs {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	b.WriteString("\r\n")

	c.logf("--> %s %s%s", req.Method, req.URL.Host, target)
	if req.Range != nil {
		c.logf("    Range: %s", req.Range.String())
	}
	if _, err := io.WriteString(cn.c, b.String()); err != nil {
		return err
	}
	if req.Body != nil {
		if _, err := cn.c.Write(req.Body); err != nil {
			return err
		}
	}
	return nil
}

func readResponse(br *bufio.Reader, req *Request) (*Response, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("malformed status line %q", line)
	}
	code, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("malformed status code in %q", line)
	}
	resp := &Response{
		Proto:         parts[0],
		StatusCode:    code,
		Header:        http.Header{},
		ContentLength: -1,
		remain:        -1,
		br:            br,
	}
	if len(parts) > 2 {
		resp.Reason = parts[2]
	}
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if l == "\r\n" || l == "\n" {
			break
		}
		l = strings.TrimRight(l, "\r\n")
		i := strings.IndexByte(l, ':')
		if i <= 0 {
			continue
		}
		name := strings.TrimSpace(l[:i])
		val := strings.TrimSpace(l[i+1:])
		resp.Header.Add(name, val)
	}

	if v := resp.Header.Get("Content-Length"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			resp.ContentLength = n
		}
	}
	if v := resp.Header.Get("Content-Range"); v != "" {
		resp.ContentRange = ParseContentRange(v)
		if resp.ContentRange != nil {
			resp.ContentLength = resp.ContentRange.End - resp.ContentRange.Start + 1
		}
	}
	if strings.EqualFold(resp.Header.Get("Accept-Ranges"), "bytes") {
		resp.AcceptRanges = true
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		resp.Filename = FilenameFromDisposition(cd)
	}
	te := strings.ToLower(resp.Header.Get("Transfer-Encoding"))
	resp.chunked = strings.Contains(te, "chunked")

	switch {
	case req.Method == "HEAD", code == 204, code == 304:
		resp.noBody = true
		resp.eof = true
	case resp.chunked:
		resp.remain = -1
	case resp.ContentLength >= 0:
		resp.remain = resp.ContentLength
	default:
		resp.remain = -1
	}
	return resp, nil
}

func parseStatusCode(line string) int {
	f := strings.Fields(line)
	if len(f) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(f[1])
	return n
}

// ParseContentRange parses "bytes 123-456/789".
func ParseContentRange(v string) *ContentRange {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(strings.ToLower(v), "bytes") {
		return nil
	}
	v = strings.TrimSpace(v[5:])
	slash := strings.IndexByte(v, '/')
	if slash < 0 {
		return nil
	}
	rng, total := v[:slash], v[slash+1:]
	dash := strings.IndexByte(rng, '-')
	if dash < 0 {
		return nil
	}
	s, err1 := strconv.ParseInt(strings.TrimSpace(rng[:dash]), 10, 64)
	e, err2 := strconv.ParseInt(strings.TrimSpace(rng[dash+1:]), 10, 64)
	if err1 != nil || err2 != nil {
		return nil
	}
	cr := &ContentRange{Start: s, End: e}
	if t, err := strconv.ParseInt(strings.TrimSpace(total), 10, 64); err == nil {
		cr.Total = t
		cr.HasTotal = true
	}
	return cr
}

// FilenameFromDisposition extracts filename/filename* from Content-Disposition,
// handling the RFC2047 and RFC5987 shapes seen in the binary.
func FilenameFromDisposition(cd string) string {
	low := strings.ToLower(cd)
	if i := strings.Index(low, "filename*="); i >= 0 {
		v := cd[i+len("filename*="):]
		if j := strings.IndexByte(v, ';'); j >= 0 {
			v = v[:j]
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		// RFC5987: charset'lang'percent-encoded
		if parts := strings.SplitN(v, "'", 3); len(parts) == 3 {
			v = parts[2]
		}
		if dec, err := url.PathUnescape(v); err == nil {
			v = dec
		}
		if v != "" {
			return sanitizeFilename(v)
		}
	}
	if i := strings.Index(low, "filename="); i >= 0 {
		v := cd[i+len("filename="):]
		if j := strings.IndexByte(v, ';'); j >= 0 {
			v = v[:j]
		}
		v = strings.Trim(strings.TrimSpace(v), `"`)
		v = DecodeRFC2047(v)
		if v != "" {
			return sanitizeFilename(v)
		}
	}
	return ""
}

func sanitizeFilename(s string) string {
	bad := `\/:*?"<>|` + "\r\n\t"
	out := strings.Map(func(r rune) rune {
		if strings.ContainsRune(bad, r) {
			return '_'
		}
		return r
	}, s)
	out = strings.TrimSpace(out)
	if out == "." || out == ".." {
		return ""
	}
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }
