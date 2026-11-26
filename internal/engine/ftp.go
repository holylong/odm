package engine

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"odm/internal/config"
	"odm/internal/ftp"
)

// IsFTP reports whether a task uses the FTP path.
func (d *Downloader) IsFTP() bool {
	u, err := url.Parse(d.Task.URL)
	return err == nil && strings.EqualFold(u.Scheme, "ftp")
}

// runFTP downloads over FTP. FTP has no byte-range header, so the transfer is a
// single connection resumed with REST <offset> — the same guarantee the HTTP
// segmented path provides, at one connection.
func (d *Downloader) runFTP() error {
	t := d.Task
	s := config.Current()
	host, port, path, user, pass, err := ftp.ParseURL(t.URL)
	if err != nil {
		return err
	}
	// Settings may carry dedicated FTP credentials.
	if user == "" && s.Proxy.FtpUserName != "" {
		user, pass = s.Proxy.FtpUserName, s.Proxy.FtpPasword
	}

	t.mu.Lock()
	t.Status = StatusProbing
	t.mu.Unlock()

	c, err := ftp.Dial(host, port, user, pass, s.Proxy.FtpPasive, time.Duration(s.TimeoutSec)*time.Second, d.logf)
	if err != nil {
		return err
	}
	defer c.Close()

	size, serr := c.Size(path)
	if serr != nil {
		d.logf("SIZE unsupported on this server: %v", serr)
	}
	name := t.Filename
	if name == "" {
		name = FilenameFromURL(path)
	}
	t.mu.Lock()
	t.Size = size
	t.Filename = name
	t.ResumeSupport = size > 0
	t.SingleConn = true
	t.Connections = 1
	t.Status = StatusDownloading
	if len(t.Segments) == 0 {
		end := size - 1
		if end < 0 {
			end = 0
		}
		t.Segments = []Segment{{Start: 0, End: end, Pos: 0}}
	}
	offset := t.Segments[0].Pos
	t.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(t.TempPath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(t.TempPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		_ = f.Sync()
		_ = f.Close()
	}()

	data, err := c.Retr(path, offset)
	if err != nil {
		return err
	}
	defer data.Close()

	stopSave := make(chan struct{})
	go func() {
		tk := time.NewTicker(time.Second)
		defer tk.Stop()
		for {
			select {
			case <-stopSave:
				return
			case <-d.stop:
				return
			case <-tk.C:
				_ = t.Save()
			}
		}
	}()
	defer close(stopSave)

	pos := offset
	buf := make([]byte, 64*1024)
	for {
		if d.isStopped() {
			return ErrStopped
		}
		n, rerr := data.Read(buf)
		if n > 0 {
			if _, werr := f.WriteAt(buf[:n], pos); werr != nil {
				return fmt.Errorf("cannot write to the temporary file: %w", werr)
			}
			pos += int64(n)
			t.mu.Lock()
			t.Segments[0].Pos = pos
			t.Downloaded = t.RecomputeDownloaded()
			t.mu.Unlock()
			d.limiter.Throttle(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("ftp transfer interrupted at %d: %w", pos, rerr)
		}
	}

	t.mu.Lock()
	t.Segments[0].Pos = t.Segments[0].End + 1
	t.Segments[0].Done = true
	if t.Size <= 0 {
		t.Size = pos
	}
	t.mu.Unlock()

	if err := f.Truncate(pos); err != nil {
		return err
	}
	if err := os.MkdirAll(t.Dir, 0o755); err != nil {
		return err
	}
	dst := uniquePath(filepath.Join(t.Dir, t.Filename))
	if err := moveFile(t.TempPath, dst); err != nil {
		return fmt.Errorf("cannot assemble the file at %s: %w", dst, err)
	}
	t.mu.Lock()
	t.Filename = filepath.Base(dst)
	t.MarkComplete()
	t.mu.Unlock()
	_ = os.Remove(t.MetaPath)
	d.logf("ftp complete: %s", dst)
	return nil
}

// ErrNoFTP is returned when an FTP URL cannot be parsed.
var ErrNoFTP = errors.New("invalid ftp URL")
