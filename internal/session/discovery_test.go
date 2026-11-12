package session

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/DR1N0/lanlobby-cli/internal/wire"
)

type failingDiscovery struct {
	fakeNetwork
	failures chan error
}
type failingAdvertisement struct {
	io.Closer
	failures <-chan error
}

func (a failingAdvertisement) Errors() <-chan error { return a.failures }
func (n *failingDiscovery) Announce(p wire.Peer) (io.Closer, error) {
	ad, err := n.fakeNetwork.Announce(p)
	if err != nil {
		return nil, err
	}
	return failingAdvertisement{Closer: ad, failures: n.failures}, nil
}

func TestFatalDiscoveryReleasesMembership(t *testing.T) {
	n := &failingDiscovery{failures: make(chan error, 1)}
	dir := testDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan wire.Peer, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Room: "r", User: "u", Network: n, RuntimeDir: dir, DiscoveryWindow: time.Millisecond, Ready: func(p wire.Peer) { ready <- p }})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("startup: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("not ready")
	}
	want := errors.New("all injected responders failed")
	n.failures <- want
	select {
	case err := <-done:
		if !errors.Is(err, want) || !strings.Contains(err.Error(), "LAN discovery unavailable") {
			t.Fatalf("lost discovery failure: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("failed discovery did not stop join")
	}
	n.mu.Lock()
	count := len(n.peers)
	n.mu.Unlock()
	if count != 0 {
		t.Fatal("advertisement was not withdrawn")
	}
	if err := Send(context.Background(), dir, "r", "u", "after leave"); err == nil {
		t.Fatal("failed join still owns send gate")
	}
	// The identical local namespace must be available to a fresh join.
	again := start(t, &n.fakeNetwork, dir, "r", "u")
	again.stop()
}
