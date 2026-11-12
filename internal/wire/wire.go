// Package wire implements the bounded, single-request TCP and local IPC protocol.
package wire

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	Version  = 1
	MaxFrame = 64 * 1024
	MaxBody  = 8 * 1024
	Timeout  = 2 * time.Second
)

// Peer describes a single foreground join, not a durable identity.
type Peer struct {
	ID        string
	Room      string
	User      string
	Port      int
	Addresses []string
}

func NewID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func ValidID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 16 && id == strings.ToLower(id)
}

func Identifier(value string) error {
	if len(value) == 0 || len(value) > 64 || !utf8.ValidString(value) {
		return errors.New("must be 1–64 UTF-8 bytes")
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			return errors.New("must not contain whitespace or control characters")
		}
	}
	return nil
}

func Body(value string) error {
	if value == "" || len(value) > MaxBody || !utf8.ValidString(value) {
		return fmt.Errorf("message must be 1–%d UTF-8 bytes", MaxBody)
	}
	return nil
}

func (p Peer) Validate() error {
	if !ValidID(p.ID) || p.Port < 1 || p.Port > 65535 {
		return errors.New("invalid peer session or port")
	}
	if err := Identifier(p.Room); err != nil {
		return fmt.Errorf("invalid room: %w", err)
	}
	if err := Identifier(p.User); err != nil {
		return fmt.Errorf("invalid user: %w", err)
	}
	return nil
}

// Frame is one newline-terminated JSON object. A connection carries one request
// and one response; no message history is exchanged during a handshake.
type Frame struct {
	Version int    `json:"v"`
	Type    string `json:"type"`
	Room    string `json:"room,omitempty"`
	User    string `json:"user,omitempty"`
	Session string `json:"session,omitempty"`
	Target  string `json:"target,omitempty"`
	Port    int    `json:"port,omitempty"`
	Body    string `json:"body,omitempty"`
	Error   string `json:"error,omitempty"`
}

func Read(r io.Reader) (Frame, error) {
	var f Frame
	b, err := bufio.NewReader(io.LimitReader(r, MaxFrame+1)).ReadBytes('\n')
	if len(b) > MaxFrame {
		return f, errors.New("frame exceeds size limit")
	}
	if err != nil {
		return f, fmt.Errorf("incomplete frame: %w", err)
	}
	if !utf8.Valid(b) {
		return f, errors.New("frame is not UTF-8")
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return f, fmt.Errorf("invalid frame: %w", err)
	}
	if f.Version != Version {
		return f, errors.New("unsupported protocol version")
	}
	return f, nil
}

func Write(w io.Writer, f Frame) error {
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

// Exchange respects caller cancellation, including when a peer stops reading.
// The caller chooses its deadline; calls without one get the protocol timeout.
func Exchange(ctx context.Context, network, address string, f Frame) (Frame, error) {
	return ExchangeWithDial(ctx, func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}, f)
}

// ExchangeWithDial applies the same bounded protocol to TCP, Unix sockets, and
// Windows named pipes. It owns and closes the connection returned by dial.
func ExchangeWithDial(ctx context.Context, dial func(context.Context) (net.Conn, error), f Frame) (Frame, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, Timeout)
		defer cancel()
	}
	conn, err := dial(ctx)
	if err != nil {
		return Frame{}, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return Frame{}, err
	}
	if err := Write(conn, f); err != nil {
		return Frame{}, err
	}
	return Read(conn)
}

// Probe verifies that an advertisement still points at the exact live session.
func Probe(ctx context.Context, p Peer) error {
	_, err := Contact(ctx, p, Frame{Version: Version, Type: "probe", Room: p.Room, Target: p.ID}, "probe_ack")
	return err
}

func Contact(ctx context.Context, p Peer, request Frame, responseType string) (Frame, error) {
	if err := p.Validate(); err != nil {
		return Frame{}, err
	}
	var last error = errors.New("peer has no usable IPv4 address")
	for _, address := range p.Addresses {
		ip := net.ParseIP(address)
		if ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsMulticast() {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, Timeout)
		response, err := Exchange(callCtx, "tcp4", net.JoinHostPort(address, strconv.Itoa(p.Port)), request)
		cancel()
		if err != nil {
			// Probes may try another interface. Never retry a chat message:
			// its acknowledgment may have been lost after delivery.
			last = err
			if request.Type == "message" {
				return Frame{}, fmt.Errorf("delivery uncertain: %w", err)
			}
			continue
		}
		if response.Error != "" {
			return Frame{}, errors.New(response.Error)
		}
		if response.Type != responseType || response.Session != p.ID || response.Room != p.Room || response.User != p.User || response.Port != p.Port {
			return Frame{}, errors.New("peer session changed or invalid acknowledgment")
		}
		return response, nil
	}
	return Frame{}, last
}

// Reachable selects the verified interface before sending a chat message. This
// avoids retrying chat data while still supporting peers with several NICs.
func Reachable(ctx context.Context, p Peer) (Peer, error) {
	var last error = errors.New("peer has no usable address")
	for _, address := range p.Addresses {
		candidate := p
		candidate.Addresses = []string{address}
		probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		err := Probe(probeCtx, candidate)
		cancel()
		if err != nil {
			last = err
			continue
		}
		return candidate, nil
	}
	return Peer{}, last
}

// SafeText keeps stdout plain and prevents messages from injecting terminal
// controls. Newlines are rendered visibly so every message remains one line.
func SafeText(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case unicode.IsControl(r) || unicode.In(r, unicode.Cf) || r == ' ' || r == ' ':
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
