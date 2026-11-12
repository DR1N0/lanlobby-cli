package discovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DR1N0/lanlobby-cli/internal/wire"
	"golang.org/x/net/ipv4"
)

type lanInterface struct {
	iface net.Interface
	ip    net.IP
}

type udpSocket interface {
	ReadFromUDP([]byte) (int, *net.UDPAddr, error)
	WriteToUDP([]byte, *net.UDPAddr) (int, error)
	SetWriteDeadline(time.Time) error
	Close() error
	LocalAddr() net.Addr
}

// Multicast is configuration only. Every Announce/Browse owns its sockets,
// including concurrent browses on the same instance.
type Multicast struct {
	ifaces       []lanInterface
	localIPs     map[string]bool
	group        *net.UDPAddr
	events       chan<- Event
	openReceiver func(lanInterface) (udpSocket, error)
	openSender   func(lanInterface) (udpSocket, error)
}

func New(interfaceName string, events ...chan<- Event) (*Multicast, error) {
	all, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	m := &Multicast{localIPs: make(map[string]bool), group: &net.UDPAddr{IP: net.ParseIP(Group), Port: Port}}
	if len(events) > 0 {
		m.events = events[0]
	}
	for _, iface := range all {
		if interfaceName != "" && iface.Name != interfaceName {
			continue
		}
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			report(m.events, "warning", "interface=%q addresses: %v", iface.Name, err)
			continue
		}
		var chosen net.IP
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err == nil && ip.To4() != nil && !ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsMulticast() {
				m.localIPs[ip.String()] = true
				if chosen == nil {
					chosen = ip.To4()
				}
			}
		}
		if chosen != nil {
			m.ifaces = append(m.ifaces, lanInterface{iface: iface, ip: chosen})
			report(m.events, "debug", "selected interface=%q index=%d IPv4=%s group=%s", iface.Name, iface.Index, chosen, m.group)
		}
	}
	if len(m.ifaces) == 0 {
		return nil, fmt.Errorf("no active IPv4 multicast LAN interface %q (check --interface and connectivity)", interfaceName)
	}
	m.openReceiver = func(i lanInterface) (udpSocket, error) {
		// Go sets reusable-port options before binding wildcard, joins the
		// group, and sets IP_MULTICAST_IF on macOS, Linux, and Windows.
		c, err := net.ListenMulticastUDP("udp4", &i.iface, m.group)
		if err != nil {
			return nil, err
		}
		if err = configure(c, i.iface); err != nil {
			_ = c.Close()
			return nil, err
		}
		return c, nil
	}
	m.openSender = func(i lanInterface) (udpSocket, error) {
		// Bind source IPv4 as well: multicast egress selection alone does
		// not choose the source IP of a unicast query response.
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: i.ip})
		if err != nil {
			return nil, err
		}
		if err = configure(c, i.iface); err != nil {
			_ = c.Close()
			return nil, err
		}
		return c, nil
	}
	return m, nil
}

func configure(c *net.UDPConn, iface net.Interface) error {
	p := ipv4.NewPacketConn(c)
	if err := p.SetMulticastInterface(&iface); err != nil {
		return fmt.Errorf("set multicast interface: %w", err)
	}
	if err := p.SetMulticastTTL(1); err != nil {
		return fmt.Errorf("set multicast TTL: %w", err)
	}
	if err := p.SetMulticastLoopback(true); err != nil {
		return fmt.Errorf("enable multicast loopback: %w", err)
	}
	return nil
}

func (m *Multicast) Diagnostics() chan<- Event { return m.events }

// Never advertise loopback, or substitute it for a genuinely remote peer.
func (m *Multicast) preferLoopback(p wire.Peer) wire.Peer {
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

type endpoint struct {
	lan              lanInterface
	receiver, sender udpSocket
}

type advertisement struct {
	cancel    context.CancelFunc
	endpoints []endpoint
	wg        sync.WaitGroup
	once      sync.Once
	errors    chan error
	remaining atomic.Int32
	limiter   replyLimiter
}

func (a *advertisement) Errors() <-chan error { return a.errors }
func (a *advertisement) Close() error {
	a.once.Do(func() {
		a.cancel()
		for _, e := range a.endpoints {
			_ = e.receiver.Close()
			_ = e.sender.Close()
		}
		a.wg.Wait()
	})
	return nil
}

// A global per-membership token bucket bounds replies without an unbounded
// source-address cache. Retries and simultaneous clients on one IP remain valid.
type replyLimiter struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func (l *replyLimiter) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last.IsZero() {
		l.tokens = 128
	} else {
		l.tokens = min(128, l.tokens+now.Sub(l.last).Seconds()*128)
	}
	l.last = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

func writeDatagram(ctx context.Context, c udpSocket, b []byte, to *net.UDPAddr) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	deadline := time.Now().Add(150 * time.Millisecond)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	if err := c.SetWriteDeadline(deadline); err != nil {
		return err
	}
	n, err := c.WriteToUDP(b, to)
	if err == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return err
}

func (m *Multicast) Announce(p wire.Peer) (io.Closer, error) {
	if _, err := encode(announcement("00000000000000000000000000000000", p)); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &advertisement{cancel: cancel, errors: make(chan error, 1)}
	var setupErrors []error
	for _, i := range m.ifaces {
		r, err := m.openReceiver(i)
		if err != nil {
			setupErrors = append(setupErrors, fmt.Errorf("%s receive: %w", i.iface.Name, err))
			report(m.events, "warning", "interface=%q join multicast: %v", i.iface.Name, err)
			continue
		}
		s, err := m.openSender(i)
		if err != nil {
			_ = r.Close()
			setupErrors = append(setupErrors, fmt.Errorf("%s reply: %w", i.iface.Name, err))
			report(m.events, "warning", "interface=%q bind reply socket: %v", i.iface.Name, err)
			continue
		}
		a.endpoints = append(a.endpoints, endpoint{lan: i, receiver: r, sender: s})
		report(m.events, "debug", "responder interface=%q IPv4=%s listen=%s replies=%s session=%s", i.iface.Name, i.ip, r.LocalAddr(), s.LocalAddr(), p.ID)
	}
	if len(a.endpoints) == 0 {
		cancel()
		return nil, fmt.Errorf("start LAN discovery responders: %w", errors.Join(append(setupErrors, errors.New("no usable interface"))...))
	}
	a.remaining.Store(int32(len(a.endpoints)))
	for _, e := range a.endpoints {
		a.wg.Add(1)
		go func(e endpoint) {
			defer a.wg.Done()
			err := m.respond(ctx, a, e, p)
			if err != nil {
				report(m.events, "warning", "responder interface=%q stopped: %v", e.lan.iface.Name, err)
			}
			if a.remaining.Add(-1) == 0 && ctx.Err() == nil {
				a.errors <- fmt.Errorf("all LAN discovery responders stopped: %w", err)
			}
		}(e)
	}
	return a, nil
}

func (m *Multicast) respond(ctx context.Context, a *advertisement, e endpoint, p wire.Peer) error {
	// Receive the entire UDP payload before enforcing MaxDatagram. In
	// particular, oversized packets must not kill Windows readers (WSAEMSGSIZE).
	b := make([]byte, 65536)
	for {
		n, src, err := e.receiver.ReadFromUDP(b)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		q, err := decode(b[:n])
		if err != nil || q.Type != "query" || !usableSource(src) {
			continue
		}
		if !a.limiter.allow(time.Now()) {
			continue
		}
		reply, err := encode(announcement(q.Query, p))
		if err != nil {
			return err
		}
		if err := writeDatagram(ctx, e.sender, reply, src); err != nil {
			if ctx.Err() == nil {
				report(m.events, "warning", "reply interface=%q to=%s query=%s: %v", e.lan.iface.Name, src, q.Query, err)
			}
		} else {
			report(m.events, "debug", "reply interface=%q to=%s query=%s session=%s", e.lan.iface.Name, src, q.Query, p.ID)
		}
	}
}

type received struct {
	peer wire.Peer
	err  error
}

func (m *Multicast) Browse(parent context.Context, emit func(wire.Peer)) error {
	if parent.Err() != nil {
		return nil
	}
	id, err := wire.NewID()
	if err != nil {
		return err
	}
	query, err := encode(queryPacket(id))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	var sockets []endpoint
	var wg sync.WaitGroup
	var packets, rejected atomic.Int64
	seen := make(map[string]wire.Peer)
	defer func() {
		cancel()
		for _, e := range sockets {
			_ = e.sender.Close()
		}
		wg.Wait()
		report(m.events, "debug", "browse query=%s packets=%d rejected=%d candidates=%d", id, packets.Load(), rejected.Load(), len(seen))
	}()
	var failures []error
	for _, i := range m.ifaces {
		c, err := m.openSender(i)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", i.iface.Name, err))
			report(m.events, "warning", "query interface=%q bind: %v", i.iface.Name, err)
			continue
		}
		sockets = append(sockets, endpoint{lan: i, sender: c})
	}
	if len(sockets) == 0 {
		return fmt.Errorf("start LAN query: %w", errors.Join(append(failures, errors.New("no usable interface"))...))
	}
	results := make(chan received, 64)
	for _, e := range sockets {
		wg.Add(1)
		go func(e endpoint) {
			defer wg.Done()
			b := make([]byte, 65536)
			for {
				n, src, err := e.sender.ReadFromUDP(b)
				if err != nil {
					if ctx.Err() == nil {
						select {
						case results <- received{err: fmt.Errorf("%s receive: %w", e.lan.iface.Name, err)}:
						case <-ctx.Done():
						}
					}
					return
				}
				packets.Add(1)
				q, err := decode(b[:n])
				if err != nil {
					rejected.Add(1)
					continue
				}
				p, ok := q.peer(id, src)
				if !ok {
					rejected.Add(1)
					continue
				}
				select {
				case results <- received{peer: p}:
				case <-ctx.Done():
					return
				}
			}
		}(e)
	}
	send := func() int {
		succeeded := 0
		for _, e := range sockets {
			if err := writeDatagram(ctx, e.sender, query, m.group); err != nil {
				failures = append(failures, fmt.Errorf("%s send: %w", e.lan.iface.Name, err))
				report(m.events, "warning", "query interface=%q destination=%s: %v", e.lan.iface.Name, m.group, err)
			} else {
				succeeded++
				report(m.events, "debug", "query=%s interface=%q source=%s destination=%s", id, e.lan.iface.Name, e.sender.LocalAddr(), m.group)
			}
		}
		return succeeded
	}
	start := time.Now()
	if send() == 0 {
		if parent.Err() != nil {
			return nil
		}
		return fmt.Errorf("all LAN query sends failed: %w", errors.Join(failures...))
	}
	retry := time.NewTimer(max(0, time.Until(start.Add(300*time.Millisecond))))
	defer retry.Stop()
	retryC := retry.C
	retries := 0
	remaining := len(sockets)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-retryC:
			send()
			retries++
			if retries == 1 {
				retry.Reset(max(0, time.Until(start.Add(900*time.Millisecond))))
			} else {
				retryC = nil
			}
		case r := <-results:
			if r.err != nil {
				failures = append(failures, r.err)
				report(m.events, "warning", "query=%s reader stopped: %v", id, r.err)
				remaining--
				if remaining == 0 {
					return fmt.Errorf("all LAN query readers stopped: %w", errors.Join(failures...))
				}
				continue
			}
			if ctx.Err() != nil {
				return nil
			}
			previous := len(seen[r.peer.ID].Addresses)
			if mergeCandidate(seen, r.peer) && len(seen[r.peer.ID].Addresses) > previous {
				p := m.preferLoopback(seen[r.peer.ID])
				report(m.events, "debug", "candidate query=%s room=%q user=%q session=%s source=%s TCP=%d", id, p.Room, p.User, p.ID, r.peer.Addresses[0], p.Port)
				emit(p)
			}
		}
	}
}
