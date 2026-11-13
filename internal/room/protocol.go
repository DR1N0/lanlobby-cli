// Package room implements a serverless LAN room: one foreground node per
// machine (serve) plus lightweight clients (join, say, tail, peers) that talk
// to it over TCP. Nodes connect to each other explicitly; nothing relies on
// multicast discovery.
package room

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
	"unicode/utf8"
)

const (
	Version = 1

	MaxFrame = 64 * 1024
	MaxBody  = 8 * 1024

	HandshakeTimeout = 5 * time.Second
	PingEvery        = 10 * time.Second
	IdleTimeout      = 35 * time.Second
	DialTimeout      = 5 * time.Second
	RequestTimeout   = 3 * time.Second
)

// Frame types.
const (
	TypeHello   = "hello"
	TypeWelcome = "welcome"
	TypeMessage = "message"
	TypeAck     = "ack"
	TypeLeave   = "leave"
	TypePing    = "ping"
	TypePong    = "pong"
	TypeMembers = "members"
	TypeNotice  = "notice"
	TypeError   = "error"
)

// Roles carried by hello.
const (
	RoleClient = "client"
	RolePeer   = "peer"
)

// Member is one live participant: a joined client or a peer node.
type Member struct {
	User    string `json:"user"`
	Session string `json:"session"`
	Peer    bool   `json:"peer,omitempty"`
}

// Frame is one newline-terminated JSON object. Every connection carries
// frames in both directions after a hello/welcome handshake.
type Frame struct {
	Version   int      `json:"v"`
	Type      string   `json:"type"`
	Room      string   `json:"room,omitempty"`
	User      string   `json:"user,omitempty"`
	Session   string   `json:"session,omitempty"`
	Host      string   `json:"host,omitempty"`
	Role      string   `json:"role,omitempty"`
	Ephemeral bool     `json:"ephemeral,omitempty"`
	Origin    string   `json:"origin,omitempty"`
	Seq       int64    `json:"seq,omitempty"`
	Body      string   `json:"body,omitempty"`
	Members   []Member `json:"members,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// SanitizeName mirrors the old CLI rules: printable, no whitespace or
// control characters, bounded length.
func SanitizeName(s string) string {
	if s == "" {
		return ""
	}
	b := make([]rune, 0, len(s))
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		b = append(b, r)
	}
	s = string(b)
	if len(s) > 64 {
		s = string([]rune(s)[:64])
	}
	return s
}

func (f Frame) Validate() error {
	if f.Version != Version {
		return errors.New("unsupported protocol version")
	}
	if !utf8.ValidString(f.Body) {
		return errors.New("body is not UTF-8")
	}
	return nil
}

// Reader is a persistent frame reader. One buffer per connection is required:
// creating a fresh bufio.Reader per frame would silently discard bytes that
// arrived in the same TCP segment.
type reader struct {
	r *bufio.Reader
}

func newReader(r io.Reader) *reader {
	return &reader{r: bufio.NewReaderSize(r, MaxFrame)}
}

// Read reads a single frame. The buffer is bounded, and oversized or
// truncated input is rejected rather than silently accepted.
func (rd *reader) Read() (Frame, error) {
	var f Frame
	b, err := rd.r.ReadBytes('\n')
	if len(b) > MaxFrame {
		return f, errors.New("frame exceeds size limit")
	}
	if err != nil {
		return f, fmt.Errorf("incomplete frame: %w", err)
	}
	if !utf8.Valid(b) {
		return f, errors.New("frame is not UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return f, fmt.Errorf("invalid frame: %w", err)
	}
	if err := f.Validate(); err != nil {
		return f, err
	}
	return f, nil
}

// Write writes a single newline-terminated frame.
func writeFrame(w io.Writer, f Frame) error {
	f.Version = Version
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if len(b) > MaxFrame {
		return errors.New("frame exceeds size limit")
	}
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
