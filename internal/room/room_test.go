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
