package discovery

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/DR1N0/lanlobby-cli/internal/wire"
	"github.com/grandcat/zeroconf"
)

// Exercise the real discovery library separately from TCP liveness, so a failed
// integration run can distinguish multicast reception from application parsing.
func TestRealMDNSAdvertisement(t *testing.T) {
	if os.Getenv("LANLOBBY_LAN_TEST") != "1" {
		t.Skip("requires real LAN multicast")
	}
	n, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	id, err := wire.NewID()
	if err != nil {
		t.Fatal(err)
	}
	ad, err := n.Announce(wire.Peer{ID: id, Room: "discovery-test", User: "probe", Port: 12345})
	if err != nil {
		t.Fatal(err)
	}
	defer ad.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, err := zeroconf.NewResolver(zeroconf.SelectIPTraffic(zeroconf.IPv4), zeroconf.SelectIfaces(n.ifaces))
	if err != nil {
		t.Fatal(err)
	}
	entries := make(chan *zeroconf.ServiceEntry, 64)
	if err := r.Browse(ctx, Service, "local.", entries); err != nil {
		t.Fatal(err)
	}
	found := false
	for e := range entries {
		t.Logf("raw advertisement: %+v", e)
		p, ok := parse(e)
		if ok && p.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("own advertisement not returned as a valid IPv4 LAN membership")
	}
}
