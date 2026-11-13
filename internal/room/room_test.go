package room

import (
	"context"
	"testing"
	"time"
)

func startServer(t *testing.T, s *Server) string {
	t.Helper()
	ready := make(chan string, 1)
	s.Listen = "127.0.0.1:0"
	s.OnReady = func(addr string) { ready <- addr }
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = s.Run(ctx) }()
	select {
	case addr := <-ready:
		return addr
	case <-time.After(3 * time.Second):
		t.Fatal("server did not start")
		return ""
	}
}

func TestClientRoundTrip(t *testing.T) {
	addr := startServer(t, &Server{Room: "dev", Name: "node"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	alice, err := Dial(ctx, addr, "dev", "alice")
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	bob, err := Dial(ctx, addr, "dev", "bob")
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()

	got := make(chan Frame, 1)
	go func() {
		_ = bob.Receive(ctx, func(f Frame) {
			if f.Type == TypeMessage {
				select {
				case got <- f:
				default:
				}
			}
		})
	}()

	if _, err := alice.Send("hello"); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-got:
		if f.User != "alice" || f.Body != "hello" {
			t.Fatalf("unexpected frame %+v", f)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("message was not relayed")
	}
}

func TestPeersRelay(t *testing.T) {
	s1 := &Server{Room: "dev", Name: "node1"}
	addr1 := startServer(t, s1)
	s2 := &Server{Room: "dev", Name: "node2", Peers: []string{addr1}}
	addr2 := startServer(t, s2)
	time.Sleep(600 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	alice, err := Dial(ctx, addr1, "dev", "alice")
	if err != nil {
		t.Fatal(err)
	}
	defer alice.Close()
	bob, err := Dial(ctx, addr2, "dev", "bob")
	if err != nil {
		t.Fatal(err)
	}
	defer bob.Close()

	got := make(chan Frame, 1)
	go func() {
		_ = bob.Receive(ctx, func(f Frame) {
			if f.Type == TypeMessage {
				select {
				case got <- f:
				default:
				}
			}
		})
	}()

	if _, err := alice.Send("across-nodes"); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-got:
		if f.Body != "across-nodes" {
			t.Fatalf("unexpected body %q", f.Body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("message did not cross nodes")
	}
}

func TestDuplicateNameRejected(t *testing.T) {
	addr := startServer(t, &Server{Room: "dev", Name: "node"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, err := Dial(ctx, addr, "dev", "same")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := Dial(ctx, addr, "dev", "same"); err == nil {
		t.Fatal("duplicate name was accepted")
	}
}

func TestEphemeralSendHasNoNotices(t *testing.T) {
	addr := startServer(t, &Server{Room: "dev", Name: "node"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	watcher, err := Dial(ctx, addr, "dev", "watcher")
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()

	frames := make(chan Frame, 8)
	go func() { _ = watcher.Receive(ctx, func(f Frame) { frames <- f }) }()

	sender, err := DialEphemeral(ctx, addr, "dev", "once")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sender.Send("hi"); err != nil {
		t.Fatal(err)
	}
	sender.Close()

	deadline := time.After(700 * time.Millisecond)
	notices, messages := 0, 0
loop:
	for {
		select {
		case f := <-frames:
			switch f.Type {
			case TypeNotice:
				notices++
			case TypeMessage:
				if f.Body == "hi" {
					messages++
				}
			}
		case <-deadline:
			break loop
		}
	}
	if notices != 0 {
		t.Fatalf("ephemeral send produced %d notices", notices)
	}
	if messages != 1 {
		t.Fatalf("expected one message, got %d", messages)
	}
}

func TestEphemeralMayShareName(t *testing.T) {
	addr := startServer(t, &Server{Room: "dev", Name: "node"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	watcher, err := Dial(ctx, addr, "dev", "same")
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()

	// say uses the same --as as the watcher; it must be accepted.
	sender, err := DialEphemeral(ctx, addr, "dev", "same")
	if err != nil {
		t.Fatalf("ephemeral sender with the member's name was rejected: %v", err)
	}
	defer sender.Close()
	if _, err := sender.Send("hi"); err != nil {
		t.Fatal(err)
	}

	// Two persistent joins still cannot share a name.
	if _, err := Dial(ctx, addr, "dev", "same"); err == nil {
		t.Fatal("a second persistent join with the same name was accepted")
	}
}
