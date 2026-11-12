package discovery

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/DR1N0/lanlobby-cli/internal/wire"
)

func testPeer() wire.Peer {
	return wire.Peer{ID: "0123456789abcdef0123456789abcdef", Room: "room1", User: "agent-a", Port: 12345}
}

const testQuery = "fedcba9876543210fedcba9876543210"

func TestDiscoveryProtocol(t *testing.T) {
	for _, p := range []packet{queryPacket(testQuery), announcement(testQuery, testPeer())} {
		b, err := encode(p)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decode(b)
		if err != nil || got != p {
			t.Fatalf("roundtrip: %+v %v", got, err)
		}
	}
	base, _ := encode(announcement(testQuery, testPeer()))
	for name, b := range map[string][]byte{
		"empty": nil, "truncated": base[:len(base)-1], "trailing": append(append([]byte{}, base...), []byte(" {}")...),
		"oversized":    append(append([]byte{}, base...), bytes.Repeat([]byte(" "), MaxDatagram)...),
		"invalid UTF8": append(append([]byte{}, base...), 0xff), "null": []byte("null"), "array": []byte("[]"),
		"unknown field": []byte(strings.TrimSuffix(string(base), "}") + `,"address":"1.2.3.4"}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decode(b); err == nil {
				t.Fatalf("accepted %q", b)
			}
		})
	}
	for _, change := range []func(*packet){
		func(p *packet) { p.Protocol = "localsend" }, func(p *packet) { p.Version = 99 }, func(p *packet) { p.Type = "message" },
		func(p *packet) { p.Query = "bad" }, func(p *packet) { p.ID = "bad" }, func(p *packet) { p.Room = "bad room" },
		func(p *packet) { p.User = "" }, func(p *packet) { p.Port = 0 }, func(p *packet) { p.Port = 65536 },
		func(p *packet) { p.Type = "query" },
	} {
		p := announcement(testQuery, testPeer())
		change(&p)
		b, _ := json.Marshal(p)
		if _, err := decode(b); err == nil {
			t.Errorf("accepted invalid packet %s", b)
		}
		if _, err := encode(p); err == nil {
			t.Errorf("encoded invalid packet %+v", p)
		}
	}
	src := &net.UDPAddr{IP: net.ParseIP("192.168.1.20"), Port: 54321}
	p := announcement(testQuery, testPeer())
	got, ok := p.peer(testQuery, src)
	if !ok || got.Addresses[0] != "192.168.1.20" || got.Port != 12345 {
		t.Fatalf("source-derived peer: %+v", got)
	}
	if _, ok := p.peer(testPeer().ID, src); ok {
		t.Fatal("accepted other browser's nonce")
	}
	for _, src := range []*net.UDPAddr{nil, {IP: net.IPv4zero, Port: 1}, {IP: net.ParseIP(Group), Port: 1}, {IP: net.IPv4bcast, Port: 1}, {IP: net.ParseIP("::1"), Port: 1}, {IP: net.IPv4(127, 0, 0, 1)}} {
		if _, ok := p.peer(testQuery, src); ok {
			t.Fatalf("accepted invalid source %v", src)
		}
	}
}

func TestMergeCandidates(t *testing.T) {
	seen := make(map[string]wire.Peer)
	p := testPeer()
	p.Addresses = []string{"192.168.1.1"}
	if !mergeCandidate(seen, p) {
		t.Fatal("first candidate")
	}
	p.Addresses = []string{"192.168.1.2", "192.168.1.1"}
	mergeCandidate(seen, p)
	if len(seen[p.ID].Addresses) != 2 {
		t.Fatal("did not merge unique source addresses")
	}
	p.User = "forged"
	if mergeCandidate(seen, p) {
		t.Fatal("accepted conflicting identity")
	}
	p = testPeer()
	for i := range 32 {
		p.Addresses = []string{fmt.Sprintf("192.168.1.%d", i+1)}
		mergeCandidate(seen, p)
	}
	if len(seen[p.ID].Addresses) != 16 {
		t.Fatal("address cap")
	}
	for i := range MaxPeers + 5 {
		p.ID = fmt.Sprintf("%032x", i+1)
		p.Addresses = []string{"192.168.1.2"}
		mergeCandidate(seen, p)
	}
	if len(seen) != MaxPeers {
		t.Fatal("candidate cap")
	}
	p = testPeer()
	p.Addresses = []string{"127.0.0.1"}
	mergeCandidate(seen, p) // Existing sessions remain eligible after reaching cap.
	if len(seen[p.ID].Addresses) != 16 {
		t.Fatal("address cap changed")
	}
}
