package discovery

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/DR1N0/lanlobby-cli/internal/wire"
)

func TestLoopbackOnlyForLocalAdvertisements(t *testing.T) {
	n := &Multicast{localIPs: map[string]bool{"192.168.1.2": true}}
	local := wire.Peer{Addresses: []string{"192.168.1.2"}}
	got := n.preferLoopback(local)
	if len(got.Addresses) != 2 || got.Addresses[0] != "127.0.0.1" || local.Addresses[0] != "192.168.1.2" {
		t.Fatalf("local loopback preference: %+v", got)
	}
	remote := wire.Peer{Addresses: []string{"192.168.1.3"}}
	if got := n.preferLoopback(remote); len(got.Addresses) != 1 || got.Addresses[0] != "192.168.1.3" {
		t.Fatalf("remote address rewritten: %+v", got)
	}
	local.Addresses = make([]string, 16)
	local.Addresses[0] = "192.168.1.2"
	if got := n.preferLoopback(local); len(got.Addresses) != 16 || got.Addresses[0] != "127.0.0.1" {
		t.Fatal("local preference exceeded address limit")
	}
}

type staticNetwork struct {
	peers []wire.Peer
	err   error
}

func (n staticNetwork) Announce(wire.Peer) (io.Closer, error) { return nil, errors.New("not used") }
func (n staticNetwork) Browse(ctx context.Context, emit func(wire.Peer)) error {
	if n.err != nil {
		return n.err
	}
	for _, p := range n.peers {
		emit(p)
	}
	<-ctx.Done()
	return nil
}

func probeServer(t *testing.T) (wire.Peer, func()) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	id, err := wire.NewID()
	if err != nil {
		t.Fatal(err)
	}
	p := wire.Peer{ID: id, Room: "room1", User: "agent-a", Port: ln.Addr().(*net.TCPAddr).Port, Addresses: []string{"127.0.0.2", "127.0.0.1"}}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(time.Second))
				f, err := wire.Read(c)
				if err != nil {
					return
				}
				if f.Type == "probe" && f.Target == p.ID {
					_ = wire.Write(c, wire.Frame{Version: wire.Version, Type: "probe_ack", Room: p.Room, User: p.User, Session: p.ID, Port: p.Port})
				}
			}()
		}
	}()
	var once sync.Once
	stop := func() { once.Do(func() { _ = ln.Close(); wg.Wait() }) }
	t.Cleanup(stop)
	return p, stop
}

func TestLiveSnapshotsProbeAndExpire(t *testing.T) {
	p, stop := probeServer(t)
	n := staticNetwork{peers: []wire.Peer{p, p}}
	ctx := context.Background()
	live, err := Live(ctx, n, "room1", 5*time.Millisecond)
	if err != nil || len(live) != 1 {
		t.Fatalf("duplicate or missing live peer: %+v %v", live, err)
	}
	if len(live[0].Addresses) != 1 || live[0].Addresses[0] != "127.0.0.1" {
		t.Fatal("did not select reachable NIC")
	}
	if live, err := Live(ctx, n, "other-room", 5*time.Millisecond); err != nil || len(live) != 0 {
		t.Fatal("room filter")
	}
	stop()
	if live, err := Live(ctx, n, "room1", 5*time.Millisecond); err != nil || len(live) != 0 {
		t.Fatalf("stale advertisement kept peer live: %+v %v", live, err)
	}
	want := errors.New("multicast unavailable")
	if _, err := Live(ctx, staticNetwork{err: want}, "", 5*time.Millisecond); !errors.Is(err, want) {
		t.Fatal("discovery failure hidden as empty LAN")
	}
}
