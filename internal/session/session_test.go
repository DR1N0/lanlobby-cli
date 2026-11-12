package session

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DR1N0/lanlobby-cli/internal/discovery"
	"github.com/DR1N0/lanlobby-cli/internal/wire"
)

type fakeNetwork struct {
	mu    sync.Mutex
	peers map[string]wire.Peer
}
type closeFunc func() error

func (f closeFunc) Close() error { return f() }
func (n *fakeNetwork) Announce(p wire.Peer) (io.Closer, error) {
	n.mu.Lock()
	if n.peers == nil {
		n.peers = make(map[string]wire.Peer)
	}
	p.Addresses = []string{"127.0.0.1"}
	n.peers[p.ID] = p
	n.mu.Unlock()
	return closeFunc(func() error { n.mu.Lock(); delete(n.peers, p.ID); n.mu.Unlock(); return nil }), nil
}
func (n *fakeNetwork) Browse(ctx context.Context, emit func(wire.Peer)) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		n.mu.Lock()
		peers := make([]wire.Peer, 0, len(n.peers))
		for _, p := range n.peers {
			peers = append(peers, p)
		}
		n.mu.Unlock()
		for _, p := range peers {
			emit(p)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *safeBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

type running struct {
	peer   wire.Peer
	output *safeBuffer
	stop   func()
}

func testDir(t *testing.T) string {
	t.Helper()
	base := "/tmp"
	if runtime.GOOS == "windows" {
		base = ""
	}
	dir, err := os.MkdirTemp(base, "lanlobby-session-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
func start(t *testing.T, n *fakeNetwork, dir, room, user string) running {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan wire.Peer, 1)
	done := make(chan error, 1)
	out := &safeBuffer{}
	go func() {
		done <- Run(ctx, Options{Room: room, User: user, Network: n, Output: out, RuntimeDir: dir, DiscoveryWindow: 30 * time.Millisecond, Ready: func(p wire.Peer) { ready <- p }})
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("join stopped with error: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("join shutdown hung")
			}
		})
	}
	t.Cleanup(stop)
	select {
	case p := <-ready:
		return running{p, out, stop}
	case err := <-done:
		cancel()
		once.Do(func() {})
		t.Fatalf("join failed: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("join not ready")
	}
	return running{}
}
func eventually(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestChatLifecycleAndNoHistory(t *testing.T) {
	n := &fakeNetwork{}
	dir := testDir(t)
	a := start(t, n, dir, "room1", "agent-a")
	b := start(t, n, dir, "room1", "agent-b")
	other := start(t, n, dir, "room2", "agent-c")
	ctx := context.Background()
	if err := Send(ctx, dir, "room1", "not-joined", "hello"); err == nil {
		t.Fatal("unjoined user sent")
	}
	if err := Send(ctx, dir, "room1", "agent-a", "hello"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return a.output.String() == "agent-a: hello\n" && b.output.String() == "agent-a: hello\n" })
	if got := other.output.String(); got != "" {
		t.Fatalf("room leaked: %q", got)
	}
	late := start(t, n, dir, "room1", "late")
	if got := late.output.String(); got != "" {
		t.Fatalf("late join replayed history: %q", got)
	}
	if err := Send(ctx, dir, "room1", "agent-b", "reply\x1b[31m\nnext"); err != nil {
		t.Fatal(err)
	}
	want := `agent-b: reply\u001b[31m\nnext` + "\n"
	eventually(t, func() bool {
		return strings.HasSuffix(a.output.String(), want) && strings.HasSuffix(b.output.String(), want) && late.output.String() == want
	})
	b.stop()
	if err := Send(ctx, dir, "room1", "agent-b", "after leave"); err == nil {
		t.Fatal("left user sent")
	}
	rejoined := start(t, n, dir, "room1", "agent-b")
	if rejoined.peer.ID == b.peer.ID {
		t.Fatal("rejoin reused session")
	}
	if rejoined.output.String() != "" {
		t.Fatal("rejoin replayed history")
	}
	if err := Send(ctx, dir, "room1", "agent-a", "new message"); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return rejoined.output.String() == "agent-a: new message\n" })
	if strings.Count(a.output.String(), "agent-a: hello\n") != 1 {
		t.Fatal("duplicate sender echo")
	}
	peers, err := discovery.Live(ctx, n, "room1", 20*time.Millisecond)
	if err != nil || len(peers) != 3 {
		t.Fatalf("bad live snapshot: %d %v", len(peers), err)
	}
	a.stop()
	late.stop()
	rejoined.stop()
	other.stop()
	peers, err = discovery.Live(ctx, n, "", 10*time.Millisecond)
	if err != nil || len(peers) != 0 {
		t.Fatalf("room survived last leave: %v %v", peers, err)
	}
}

func TestDuplicateAndInvalidRemoteSession(t *testing.T) {
	n := &fakeNetwork{}
	dir := testDir(t)
	a := start(t, n, dir, "r", "u")
	if err := Run(context.Background(), Options{Room: "r", User: "u", Network: n, RuntimeDir: dir}); err == nil {
		t.Fatal("duplicate local join")
	}
	a.peer.Addresses = []string{"127.0.0.1"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	wrong := wire.Frame{Version: wire.Version, Type: "message", Room: "other", Target: a.peer.ID, Body: "bad"}
	if _, err := wire.Contact(ctx, a.peer, wrong, "ack"); err == nil {
		t.Fatal("cross-room message accepted")
	}
	wrong.Room = "r"
	wrong.User = "ghost"
	wrong.Session = "00000000000000000000000000000001"
	wrong.Port = 1
	if _, err := wire.Contact(ctx, a.peer, wrong, "ack"); err == nil {
		t.Fatal("departed sender accepted")
	}
	if a.output.String() != "" {
		t.Fatal("invalid message displayed")
	}
}

func TestQueueAndConflicts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &membership{ctx: ctx, active: true, incoming: make(chan message, 1)}
	if err := s.enqueue(message{"u", "one"}); err != nil {
		t.Fatal(err)
	}
	if err := s.enqueue(message{"u", "two"}); err == nil {
		t.Fatal("unbounded queue")
	}
	cancel()
	if err := s.enqueue(message{"u", "three"}); err == nil {
		t.Fatal("cancelled session accepted")
	}
	p := wire.Peer{ID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Room: "r", User: "u"}
	lower := p
	lower.ID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if conflict(p, []wire.Peer{lower}) == nil || conflict(lower, []wire.Peer{p}) != nil {
		t.Fatal("non-deterministic name conflict")
	}
	lower.Room = "other"
	if conflict(p, []wire.Peer{lower}) != nil {
		t.Fatal("conflict across rooms")
	}
}

func TestPartialDeliveryIsReported(t *testing.T) {
	n := &fakeNetwork{}
	dir := testDir(t)
	a := start(t, n, dir, "r", "a")
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	p := wire.Peer{ID: "00000000000000000000000000000002", Room: "r", User: "rejecting", Port: ln.Addr().(*net.TCPAddr).Port}
	ad, err := n.Announce(p)
	if err != nil {
		t.Fatal(err)
	}
	defer ad.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				f, err := wire.Read(c)
				if err != nil {
					return
				}
				reply := wire.Frame{Version: wire.Version, Type: "probe_ack", Room: p.Room, User: p.User, Session: p.ID, Port: p.Port}
				if f.Type == "message" {
					reply.Type = "ack"
					reply.Error = "output queue full"
				}
				_ = wire.Write(c, reply)
			}()
		}
	}()
	err = Send(context.Background(), dir, "r", "a", "test")
	if err == nil || !strings.Contains(err.Error(), "partial or uncertain delivery") {
		t.Fatalf("missing partial error: %v", err)
	}
	eventually(t, func() bool { return a.output.String() == "a: test\n" })
}

type blockedWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}
func TestLeaveDoesNotWaitForBlockedStdout(t *testing.T) {
	n := &fakeNetwork{}
	dir := testDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan wire.Peer, 1)
	done := make(chan error, 1)
	w := &blockedWriter{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(w.release)
	go func() {
		done <- Run(ctx, Options{Room: "r", User: "u", Network: n, Output: w, RuntimeDir: dir, DiscoveryWindow: 10 * time.Millisecond, Ready: func(p wire.Peer) { ready <- p }})
	}()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("not ready")
	}
	if err := Send(context.Background(), dir, "r", "u", "test"); err != nil {
		cancel()
		t.Fatal(err)
	}
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("not writing")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("leave blocked on stdout")
	}
	if err := Send(context.Background(), dir, "r", "u", "late"); err == nil {
		t.Fatal("left membership sent")
	}
}
