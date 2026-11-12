package discovery

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/DR1N0/lanlobby-cli/internal/wire"
)

// Verify actual multicast fanout and nonce-matched unicast replies separately
// from TCP liveness. Same-host success is NOT a two-machine LAN test.
func TestRealMulticastAdvertisements(t *testing.T) {
	if os.Getenv("LANLOBBY_LAN_TEST") != "1" {
		t.Skip("requires real LAN multicast")
	}
	events := make(chan Event, 1024)
	defer func() {
		if t.Failed() {
			for {
				select {
				case e := <-events:
					t.Logf("%s: %s", e.Level, e.Message)
				default:
					return
				}
			}
		}
	}()
	n, err := New("", events)
	if err != nil {
		t.Fatal(err)
	}
	ids := make(map[string]bool)
	for range 3 {
		id, err := wire.NewID()
		if err != nil {
			t.Fatal(err)
		}
		ad, err := n.Announce(wire.Peer{ID: id, Room: "discovery-test", User: "probe", Port: 12345})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = ad.Close() })
		ids[id] = true
	}
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), Window)
			defer cancel()
			seen := make(map[string]bool)
			if err := n.Browse(ctx, func(p wire.Peer) { seen[p.ID] = true }); err != nil {
				t.Error(err)
				return
			}
			for id := range ids {
				if !seen[id] {
					t.Errorf("concurrent browser missed advertiser %s (multicast fanout or unicast response failed)", id)
				}
			}
		})
	}
	wg.Wait()
}
