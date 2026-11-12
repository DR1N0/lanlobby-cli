package wire

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestFrames(t *testing.T) {
	want := Frame{Version: Version, Type: "message", Room: "room1", Body: "hello\nworld"}
	var b bytes.Buffer
	if err := Write(&b, want); err != nil {
		t.Fatal(err)
	}
	if bytes.Count(b.Bytes(), []byte{'\n'}) != 1 {
		t.Fatal("not a single NDJSON frame")
	}
	got, err := Read(&b)
	if err != nil || got != want {
		t.Fatalf("got %+v, %v", got, err)
	}
	for _, input := range []string{"", `{"v":1}`, "{bad}\n", "{\"v\":9}\n", strings.Repeat("x", MaxFrame) + "\n", "\xff\n", "{\"v\":1}\x00\n"} {
		if _, err := Read(strings.NewReader(input)); err == nil {
			t.Errorf("accepted malformed frame %q", input[:min(len(input), 40)])
		}
	}
	if err := Write(io.Discard, Frame{Version: Version, Body: strings.Repeat("x", MaxFrame)}); err == nil {
		t.Fatal("oversized frame accepted")
	}
}

func TestValidationAndSafeText(t *testing.T) {
	for _, s := range []string{"", "two words", "escape\x1b", "bidi‮", strings.Repeat("a", 65)} {
		if Identifier(s) == nil {
			t.Errorf("accepted identifier %q", s)
		}
	}
	for _, s := range []string{"room1", "agent-a", "讨论"} {
		if err := Identifier(s); err != nil {
			t.Errorf("rejected identifier %q: %v", s, err)
		}
	}
	if Body("") == nil || Body(strings.Repeat("x", MaxBody+1)) == nil || Body("\xff") == nil {
		t.Fatal("invalid body accepted")
	}
	id, err := NewID()
	if err != nil || !ValidID(id) || ValidID("a") || ValidID(strings.ToUpper("abcdef0123456789abcdef0123456789")) {
		t.Fatal("bad session validation")
	}
	if got := SafeText("ok\x1b[31m\n\t\r‮"); got != "ok\\u001b[31m\\n\\t\\r\\u202e" {
		t.Fatalf("unsafe output %q", got)
	}
}

func TestExchangeWithCustomDial(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	want := Frame{Version: Version, Type: "ack", Body: "hello"}
	done := make(chan error, 1)
	go func() {
		defer server.Close()
		if _, err := Read(server); err != nil {
			done <- err
			return
		}
		done <- Write(server, want)
	}()
	got, err := ExchangeWithDial(context.Background(), func(ctx context.Context) (net.Conn, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("custom dialer did not receive default timeout")
		}
		return client, nil
	}, Frame{Version: Version, Type: "probe"})
	if err != nil || got != want {
		t.Fatalf("custom transport: %+v %v", got, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte("closed")); err == nil {
		t.Fatal("exchange did not close its connection")
	}
}

func TestExchangeCancelsCustomDial(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := ExchangeWithDial(ctx, func(ctx context.Context) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}, Frame{Version: Version})
	if err == nil {
		t.Fatal("canceled custom dial succeeded")
	}
}

func TestExchangeCancellation(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := Exchange(ctx, "tcp4", ln.Addr().String(), Frame{Version: Version, Type: "probe"}); err == nil {
		t.Fatal("silent peer accepted")
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancellation did not interrupt read")
	}
	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(time.Second):
		t.Fatal("no accepted connection")
	}
}
