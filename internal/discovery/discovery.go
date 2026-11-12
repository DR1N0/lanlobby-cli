// Package discovery finds ephemeral memberships, never chat messages.
package discovery

import (
	"context"
	"fmt"
	"io"
	"net"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/DR1N0/lanlobby-cli/internal/wire"
)

const (
	Window   = 1500 * time.Millisecond
	MaxPeers = 256
)

// Browse blocks until ctx expires and must stop invoking emit before returning.
// A Network has no persistent sockets: operations own their own lifetimes.
type Network interface {
	Announce(wire.Peer) (io.Closer, error)
	Browse(context.Context, func(wire.Peer)) error
}

// Event contains no chat content. Producers never wait for the consumer.
// Warning events describe degraded interfaces; debug events describe discovery.
type Event struct{ Level, Message string }

func report(events chan<- Event, level, format string, args ...any) {
	if events == nil {
		return
	}
	select {
	case events <- Event{Level: level, Message: fmt.Sprintf(format, args...)}:
	default: // Diagnostics are best effort; never stall networking or leave.
	}
}

func networkEvents(n Network) chan<- Event {
	if d, ok := n.(interface{ Diagnostics() chan<- Event }); ok {
		return d.Diagnostics()
	}
	return nil
}

type observedNetwork struct {
	Network
	events chan<- Event
}

func (n observedNetwork) Diagnostics() chan<- Event { return n.events }

// WithDiagnostics preserves the injected Network seam while observing Live.
func WithDiagnostics(n Network, events chan<- Event) Network {
	return observedNetwork{Network: n, events: events}
}

// mergeCandidate keeps all observed source addresses, but never changes the
// identity/port of an existing session. Every address still needs a TCP probe.
func mergeCandidate(candidates map[string]wire.Peer, p wire.Peer) bool {
	if p.Validate() != nil || len(p.Addresses) == 0 || len(p.Addresses) > 16 {
		return false
	}
	old, exists := candidates[p.ID]
	if !exists {
		if len(candidates) >= MaxPeers {
			return false
		}
		old = p
		old.Addresses = nil
	} else if old.Room != p.Room || old.User != p.User || old.Port != p.Port {
		return false
	}
	for _, address := range p.Addresses {
		ip := net.ParseIP(address)
		if ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsMulticast() || ip.Equal(net.IPv4bcast) {
			continue
		}
		address = ip.String()
		found := false
		for _, previous := range old.Addresses {
			if previous == address {
				found = true
				break
			}
		}
		if !found && len(old.Addresses) < 16 {
			old.Addresses = append(old.Addresses, address)
		}
	}
	if len(old.Addresses) == 0 {
		return false
	}
	candidates[p.ID] = old
	return true
}

// Live returns a bounded, fresh snapshot. Advertisements are only candidates:
// every result must answer a TCP probe for the exact advertised session.
func Live(ctx context.Context, network Network, room string, window time.Duration) ([]wire.Peer, error) {
	if window <= 0 {
		window = Window
	}
	ctx, cancel := context.WithTimeout(ctx, window+wire.Timeout)
	defer cancel()
	browseCtx, stop := context.WithTimeout(ctx, window)
	candidates := make(map[string]wire.Peer)
	var mu sync.Mutex
	events := networkEvents(network)
	err := network.Browse(browseCtx, func(p wire.Peer) {
		if room != "" && p.Room != room {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		mergeCandidate(candidates, p)
	})
	stop()
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	report(events, "debug", "snapshot room=%q candidates=%d; starting TCP verification", room, len(candidates))
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
				report(events, "debug", "TCP verified room=%q user=%q session=%s address=%s", p.Room, p.User, p.ID, Address(reachable))
			} else {
				report(events, "debug", "TCP verification failed room=%q user=%q session=%s addresses=%v port=%d: %v", p.Room, p.User, p.ID, p.Addresses, p.Port, err)
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
	report(events, "debug", "snapshot room=%q candidates=%d live=%d", room, len(candidates), len(live))
	return live, nil
}

func Address(p wire.Peer) string {
	if len(p.Addresses) == 0 {
		return "?"
	}
	return net.JoinHostPort(p.Addresses[0], strconv.Itoa(p.Port))
}
