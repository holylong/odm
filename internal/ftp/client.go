// Package ftp implements the subset of FTP needed for resumable downloads:
// USER/PASS, TYPE I, SIZE, PASV/EPSV, REST and RETR.
//
// IDM supports ftp:// URLs alongside http/https and keeps the same passive-mode
// and resume settings (FtpPasive, FtpUserName, FtpPasword in re/FINDINGS.md §4),
// so the download engine treats FTP as a single-connection, REST-resumable task.
package ftp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is a control connection plus an optional data connection.
type Client struct {
	ctrl    net.Conn
	r       *bufio.Reader
	Host    string
	Port    string
	Passive bool
	Timeout time.Duration
	Logf    func(string, ...any)
}

// ParseURL splits an ftp:// URL into host, port, path and credentials.
func ParseURL(raw string) (host, port, path, user, pass string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", "", "", "", err
	}
	if u.Scheme != "ftp" {
		return "", "", "", "", "", fmt.Errorf("not an ftp URL: %s", u.Scheme)
	}
	host = u.Hostname()
	port = u.Port()
	if port == "" {
		port = "21"
	}
	path = u.Path
	if path == "" {
		path = "/"
	}
	if u.User != nil {
		user = u.User.Username()
		pass, _ = u.User.Password()
	}
	return host, port, path, user, pass, nil
}

// Dial connects and logs in.
func Dial(host, port, user, pass string, passive bool, timeout time.Duration, logf func(string, ...any)) (*Client, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	d := &net.Dialer{Timeout: timeout}
	conn, err := d.Dial("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("cannot connect to %s:%s: %w", host, port, err)
	}
	c := &Client{ctrl: conn, r: bufio.NewReader(conn), Host: host, Port: port, Passive: passive, Timeout: timeout, Logf: logf}
	if _, _, err := c.read(); err != nil {
		conn.Close()
		return nil, err
	}
	if user == "" {
		user, pass = "anonymous", "anonymous@"
	}
	if code, msg, err := c.Cmd("USER %s", user); err != nil || code/100 != 2 && code/100 != 3 {
		conn.Close()
		return nil, fmt.Errorf("ftp login failed: %d %s", code, msg)
	}
	if code, msg, err := c.Cmd("PASS %s", pass); err != nil || code/100 != 2 {
		conn.Close()
		return nil, fmt.Errorf("ftp password rejected: %d %s", code, msg)
	}
	_, _, _ = c.Cmd("TYPE I")
	return c, nil
}

// Cmd sends one command and reads the reply (handling multi-line replies).
func (c *Client) Cmd(format string, args ...any) (int, string, error) {
	line := fmt.Sprintf(format, args...)
	c.Logf("ftp> %s", redact(line))
	_ = c.ctrl.SetDeadline(time.Now().Add(c.Timeout))
	if _, err := io.WriteString(c.ctrl, line+"\r\n"); err != nil {
		return 0, "", err
	}
	return c.read()
}

func redact(line string) string {
	if strings.HasPrefix(strings.ToUpper(line), "PASS ") {
		return "PASS ***"
	}
	return line
}

func (c *Client) read() (int, string, error) {
	var buf strings.Builder
	for {
		l, err := c.r.ReadString('\n')
		if err != nil {
			return 0, "", err
		}
		buf.WriteString(l)
		l = strings.TrimRight(l, "\r\n")
		if len(l) < 4 {
			return 0, buf.String(), errors.New("malformed FTP reply")
		}
		code, err := strconv.Atoi(l[:3])
		if err != nil {
			return 0, buf.String(), errors.New("malformed FTP status code")
		}
		// Continuation lines look like "123-text"; the final line is "123 text".
		if l[3] == '-' {
			continue
		}
		return code, buf.String(), nil
	}
}

// Size returns the remote file size via SIZE, 0 or -1 when unsupported.
func (c *Client) Size(path string) (int64, error) {
	code, msg, err := c.Cmd("SIZE %s", path)
	if err != nil {
		return -1, err
	}
	if code != 213 {
		return -1, fmt.Errorf("SIZE unsupported: %d", code)
	}
	return parseReplySize(msg)
}

func parseReplySize(msg string) (int64, error) {
	lines := strings.Split(strings.TrimSpace(msg), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	fields := strings.Fields(last)
	if len(fields) < 2 {
		return -1, errors.New("cannot parse SIZE reply")
	}
	return strconv.ParseInt(fields[1], 10, 64)
}

// Retr opens the data connection and starts a transfer at offset bytes.
func (c *Client) Retr(path string, offset int64) (io.ReadCloser, error) {
	dataAddr, err := c.passiveAddr()
	if err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: c.Timeout}
	data, err := d.Dial("tcp", dataAddr)
	if err != nil {
		return nil, fmt.Errorf("cannot open the ftp data connection: %w", err)
	}
	if offset > 0 {
		if code, msg, err := c.Cmd("REST %d", offset); err != nil || code/100 != 3 {
			data.Close()
			return nil, fmt.Errorf("server cannot resume at %d: %d %s", offset, code, msg)
		}
	}
	code, msg, err := c.Cmd("RETR %s", path)
	if err != nil || code/100 != 1 {
		data.Close()
		return nil, fmt.Errorf("RETR failed: %d %s", code, msg)
	}
	return data, nil
}

func (c *Client) passiveAddr() (string, error) {
	if c.Passive {
		if addr, ok := c.epsv(); ok {
			return addr, nil
		}
		if addr, ok := c.pasv(); ok {
			return addr, nil
		}
		return "", errors.New("server rejected passive mode")
	}
	return c.activeListen()
}

func (c *Client) epsv() (string, bool) {
	code, msg, err := c.Cmd("EPSV")
	if err != nil || code != 229 {
		return "", false
	}
	// 229 Entering Extended Passive Mode (|||6446|)
	i := strings.LastIndex(msg, "(|||")
	if i < 0 {
		return "", false
	}
	rest := msg[i+4:]
	j := strings.IndexByte(rest, '|')
	if j <= 0 {
		return "", false
	}
	port := rest[:j]
	return net.JoinHostPort(c.Host, port), true
}

func (c *Client) pasv() (string, bool) {
	code, msg, err := c.Cmd("PASV")
	if err != nil || code != 227 {
		return "", false
	}
	open := strings.IndexByte(msg, '(')
	closeIdx := strings.IndexByte(msg, ')')
	if open < 0 || closeIdx < open {
		return "", false
	}
	parts := strings.Split(msg[open+1:closeIdx], ",")
	if len(parts) != 6 {
		return "", false
	}
	nums := make([]int, 6)
	for i, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return "", false
		}
		nums[i] = n
	}
	ip := fmt.Sprintf("%d.%d.%d.%d", nums[0], nums[1], nums[2], nums[3])
	port := nums[4]*256 + nums[5]
	// Many servers return an address behind NAT; prefer the control peer.
	if ip == "0.0.0.0" || strings.HasPrefix(ip, "127.") && !strings.HasPrefix(c.Host, "127.") {
		ip = c.Host
	}
	return net.JoinHostPort(ip, strconv.Itoa(port)), true
}

func (c *Client) activeListen() (string, error) {
	l, port, err := listenLoopback()
	if err != nil {
		return "", err
	}
	defer l.Close()
	ip := c.ctrl.LocalAddr().(*net.TCPAddr).IP.String()
	p1, p2 := port/256, port%256
	ipParts := strings.Split(ip, ".")
	if len(ipParts) != 4 {
		return "", errors.New("cannot use active mode with an unusual local address")
	}
	code, msg, err := c.Cmd("PORT %s,%s,%s,%s,%d,%d", ipParts[0], ipParts[1], ipParts[2], ipParts[3], p1, p2)
	if err != nil || code/100 != 2 {
		return "", fmt.Errorf("PORT rejected: %d %s", code, msg)
	}
	return "listen:" + strconv.Itoa(port), nil
}

// Quit closes the control connection politely.
func (c *Client) Quit() {
	_, _, _ = c.Cmd("QUIT")
	_ = c.ctrl.Close()
}

// Close drops the control connection.
func (c *Client) Close() error { return c.ctrl.Close() }
