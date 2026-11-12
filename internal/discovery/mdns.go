// Package discovery finds ephemeral memberships, never chat messages.
package discovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/DR1N0/lanlobby-cli/internal/wire"
	"github.com/grandcat/zeroconf"
)

const (
	Service  = "_lanlobby._tcp"
	Window   = 1500 * time.Millisecond
	MaxPeers = 256
)

// Network abstracts discovery for deterministic tests without multicast.
// Browse blocks until ctx expires and must stop invoking emit before returning.
type Network interface {
	Announce(wire.Peer) (io.Closer, error)
	Browse(context.Context, func(wire.Peer)) error
}

type MDNS struct {
	ifaces   []net.Interface
	localIPs map[string]bool
}

func New(interfaceName string) (*MDNS, error) {
	all, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var chosen []net.Interface
	localIPs := make(map[string]bool)
	for _, iface := range all {
		if interfaceName != "" && iface.Name != interfaceName {
			continue
		}
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		usable := false
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err == nil && ip.To4() != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
				localIPs[ip.String()] = true
				usable = true
			}
		}
		if usable {
			chosen = append(chosen, iface)
		}
	}
	if len(chosen) == 0 {
		return nil, fmt.Errorf("no active IPv4 multicast LAN interface %q (check --interface and network connectivity)", interfaceName)
	}
	return &MDNS{ifaces: chosen, localIPs: localIPs}, nil
}

// A same-host peer may be unreachable through its LAN/VPN address (for example,
// due to local firewall or VPN routing). Prefer loopback only when an advertised
// address belongs to this machine. TCP probing still verifies the exact session;
// loopback is never advertised to other machines or substituted for remote IPs.
func (m *MDNS) preferLoopback(p wire.Peer) wire.Peer {
	for _, address := range p.Addresses {
		if m.localIPs[address] {
			addresses := p.Addresses
			if len(addresses) > 15 {
				addresses = addresses[:15]
			}
			p.Addresses = append([]string{"127.0.0.1"}, addresses...)
			break
		}
	}
	return p
}

type advertisement struct{ server *zeroconf.Server }

func (a *advertisement) Close() error { a.server.Shutdown(); return nil }

func (m *MDNS) Announce(p wire.Peer) (io.Closer, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	text := []string{"v=1", "id=" + p.ID, "room=" + p.Room, "user=" + p.User}
	server, err := zeroconf.Register(p.ID, Service, "local.", p.Port, text, m.ifaces)
	if err != nil {
		return nil, fmt.Errorf("announce LAN membership: %w", err)
	}
	return &advertisement{server}, nil
}

func parse(entry *zeroconf.ServiceEntry) (wire.Peer, bool) {
	if entry.TTL == 0 || len(entry.Text) != 4 {
		return wire.Peer{}, false
	}
	fields := make(map[string]string, 4)
	for _, txt := range entry.Text {
		key, value, ok := strings.Cut(txt, "=")
		if !ok {
			return wire.Peer{}, false
		}
		if _, exists := fields[key]; exists {
			return wire.Peer{}, false
		}
		fields[key] = value
	}
	p := wire.Peer{ID: fields["id"], Room: fields["room"], User: fields["user"], Port: entry.Port}
	if fields["v"] != "1" || entry.Instance != p.ID || p.Validate() != nil {
		return wire.Peer{}, false
	}
	for _, ip := range entry.AddrIPv4 {
		if ip.To4() != nil && !ip.IsUnspecified() && !ip.IsMulticast() {
			p.Addresses = append(p.Addresses, ip.String())
		}
		if len(p.Addresses) == 16 {
			break
		}
	}
	sort.Strings(p.Addresses)
	return p, len(p.Addresses) > 0
}

func (m *MDNS) Browse(parent context.Context, emit func(wire.Peer)) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	resolver, err := zeroconf.NewResolver(zeroconf.SelectIPTraffic(zeroconf.IPv4), zeroconf.SelectIfaces(m.ifaces))
	if err != nil {
		return fmt.Errorf("start LAN discovery: %w", err)
	}
	entries := make(chan *zeroconf.ServiceEntry, 64)
	if err := resolver.Browse(ctx, Service, "local.", entries); err != nil {
		cancel()
		// The library sends before checking cancellation; keep draining so its
		// sockets can shut down even when a result races cancellation.
		for range entries {
		}
		return fmt.Errorf("query LAN discovery: %w", err)
	}
	for entry := range entries {
		if ctx.Err() == nil {
			if p, ok := parse(entry); ok {
				emit(m.preferLoopback(p))
			}
		}
	}
	if parent.Err() == nil {
		return errors.New("LAN discovery stopped unexpectedly")
	}
	return nil
}

// Live returns a bounded, fresh snapshot. Advertisements are only candidates:
// every result must answer a TCP probe for the exact advertised session.
// No lease/cache is reused across calls, so stale records cannot keep users live.
func Live(ctx context.Context, network Network, room string, window time.Duration) ([]wire.Peer, error) {
	if window <= 0 {
		window = Window
	}
	ctx, cancel := context.WithTimeout(ctx, window+wire.Timeout)
	defer cancel()
	browseCtx, stop := context.WithTimeout(ctx, window)
	candidates := make(map[string]wire.Peer)
	var mu sync.Mutex
	err := network.Browse(browseCtx, func(p wire.Peer) {
		if p.Validate() != nil || len(p.Addresses) == 0 || len(p.Addresses) > 16 || (room != "" && p.Room != room) {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if _, exists := candidates[p.ID]; exists || len(candidates) < MaxPeers {
			candidates[p.ID] = p
		}
	})
	stop()
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var live []wire.Peer
	var wg sync.WaitGroup
	limit := make(chan struct{}, 16)
	for _, p := range candidates {
		select {
		case limit <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return nil, ctx.Err()
		}
		wg.Add(1)
		go func(p wire.Peer) {
			defer wg.Done()
			defer func() { <-limit }()
			if reachable, err := wire.Reachable(ctx, p); err == nil {
				mu.Lock()
				live = append(live, reachable)
				mu.Unlock()
			}
		}(p)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	sort.Slice(live, func(i, j int) bool {
		a, b := live[i], live[j]
		if a.Room != b.Room {
			return a.Room < b.Room
		}
		if a.User != b.User {
			return a.User < b.User
		}
		return a.ID < b.ID
	})
	return live, nil
}

func Address(p wire.Peer) string {
	if len(p.Addresses) == 0 {
		return "?"
	}
	return net.JoinHostPort(p.Addresses[0], strconv.Itoa(p.Port))
}
