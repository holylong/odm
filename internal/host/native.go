// Package host implements the native-messaging bridge for the browser extension.
//
// Chrome speaks a length-prefixed message protocol on stdin/stdout:
// a 4-byte little-endian length followed by one JSON document. The shipped IDM
// extension frames its own payload as ["MSG#",l,"#",type,"#",A,"#",j]
// (re/ext_chrome/background.js, see re/FINDINGS.md §7); ODM keeps the transport
// identical and uses a plain JSON body, which the extension documents.
package host

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// MaxMessage is the Chrome limit for a single native message.
const MaxMessage = 1 << 20

// Message is one request from the extension.
type Message struct {
	Type      string            `json:"type"`
	URL       string            `json:"url,omitempty"`
	Referer   string            `json:"referer,omitempty"`
	Filename  string            `json:"filename,omitempty"`
	Mime      string            `json:"mime,omitempty"`
	Size      int64             `json:"size,omitempty"`
	UserAgent string            `json:"userAgent,omitempty"`
	Cookies   map[string]string `json:"cookies,omitempty"`
	TabID     int               `json:"tabID,omitempty"`
	Silent    bool              `json:"silent,omitempty"`
}

// Reply is one response.
type Reply struct {
	OK       bool   `json:"ok"`
	App      string `json:"app,omitempty"`
	Version  string `json:"version,omitempty"`
	Error    string `json:"error,omitempty"`
	Takeover bool   `json:"takeover,omitempty"`
	Reason   string `json:"reason,omitempty"`
	TaskID   string `json:"taskID,omitempty"`
	Port     int    `json:"port,omitempty"`
}

// ReadMessage decodes one framed message from r.
func ReadMessage(r io.Reader) (*Message, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if n == 0 || n > MaxMessage {
		return nil, fmt.Errorf("native message length %d out of range", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	var m Message
	if err := json.Unmarshal(buf, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// WriteMessage frames and writes one reply to w.
func WriteMessage(w io.Writer, rep *Reply) error {
	b, err := json.Marshal(rep)
	if err != nil {
		return err
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// Serve runs the stdin/stdout loop, delegating decisions to handle.
func Serve(in io.Reader, out io.Writer, handle func(*Message) *Reply) error {
	br := bufio.NewReader(in)
	bw := bufio.NewWriter(out)
	for {
		m, err := ReadMessage(br)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		rep := handle(m)
		if rep == nil {
			rep = &Reply{OK: false, Error: "unsupported message type " + m.Type}
		}
		if err := WriteMessage(bw, rep); err != nil {
			return err
		}
		if err := bw.Flush(); err != nil {
			return err
		}
	}
}

// Stdio is the production entry point.
func Stdio(handle func(*Message) *Reply) error {
	return Serve(os.Stdin, os.Stdout, handle)
}
