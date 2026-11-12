package discovery

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DR1N0/lanlobby-cli/internal/wire"
)

// Deterministic transport tests use ordinary loopback UDP, not LAN multicast.
func loopbackNetwork(t *testing.T) (*Multicast, *net.UDPConn) {
	t.Helper()
	r, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	m := &Multicast{ifaces: []lanInterface{{iface: net.Interface{Name: "test"}, ip: net.IPv4(127, 0, 0, 1)}}, localIPs: map[string]bool{"127.0.0.1": true}, group: r.LocalAddr().(*net.UDPAddr)}
	m.openReceiver = func(lanInterface) (udpSocket, error) { return r, nil }
	m.openSender = func(lanInterface) (udpSocket, error) {
		return net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	}
	return m, r
}

func TestUnicastRepliesConcurrentBrowsesAndClose(t *testing.T) {
	m, _ := loopbackNetwork(t)
	ad, err := m.Announce(testPeer())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ad.Close() })
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
			defer cancel()
			count := 0
			if err := m.Browse(ctx, func(p wire.Peer) {
				if p.ID == testPeer().ID {
					count++
				}
			}); err != nil {
				t.Error(err)
			}
			if count != 1 {
				t.Errorf("nonce isolation/deduplication: callbacks=%d", count)
			}
		})
	}
	wg.Wait()
	if err := ad.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ad.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBrowseCancellationNoLateCallbacks(t *testing.T) {
	m, _ := loopbackNetwork(t)
	ad, err := m.Announce(testPeer())
	if err != nil {
		t.Fatal(err)
	}
	defer ad.Close()
	var opened []udpSocket
	original := m.openSender
	m.openSender = func(i lanInterface) (udpSocket, error) {
		c, err := original(i)
		if err == nil {
			opened = append(opened, c)
		}
		return c, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	var count atomic.Int32
	err = m.Browse(ctx, func(wire.Peer) { count.Add(1); cancel() })
	if err != nil || count.Load() != 1 {
		t.Fatalf("cancel: count=%d error=%v", count.Load(), err)
	}
	for _, c := range opened {
		if err := c.SetWriteDeadline(time.Now()); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("Browse returned with a live query socket: %v", err)
		}
	}
	n := count.Load()
	// Allow any incorrectly detached worker to expose a late callback.
	time.Sleep(20 * time.Millisecond)
	if n != count.Load() {
		t.Fatal("callback after return")
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if err := m.Browse(ctx, func(wire.Peer) { t.Error("callback with canceled context") }); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverySetupAndFatalReaderErrors(t *testing.T) {
	m, r := loopbackNetwork(t)
	want := errors.New("injected socket setup error")
	original := m.openSender
	m.openSender = func(lanInterface) (udpSocket, error) { return nil, want }
	if _, err := m.Announce(testPeer()); !errors.Is(err, want) {
		t.Fatalf("announcement setup failure: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := m.Browse(ctx, func(wire.Peer) {}); !errors.Is(err, want) {
		t.Fatalf("browse setup failure: %v", err)
	}
	// Announce's failed setup closed its receiver; use a fresh one.
	m, r = loopbackNetwork(t)
	m.openSender = original
	ad, err := m.Announce(testPeer())
	if err != nil {
		t.Fatal(err)
	}
	defer ad.Close()
	_ = r.Close()
	select {
	case err := <-ad.(interface{ Errors() <-chan error }).Errors():
		if err == nil {
			t.Fatal("lost all responders without error")
		}
	case <-time.After(time.Second):
		t.Fatal("terminal receive error was hidden")
	}
}

type brokenSocket struct {
	udpSocket
	sendErr, deadlineErr, readErr error
	short                         bool
}

func (s brokenSocket) ReadFromUDP(b []byte) (int, *net.UDPAddr, error) {
	if s.readErr != nil {
		return 0, nil, s.readErr
	}
	return s.udpSocket.ReadFromUDP(b)
}

func (s brokenSocket) SetWriteDeadline(t time.Time) error {
	if s.deadlineErr != nil {
		return s.deadlineErr
	}
	return s.udpSocket.SetWriteDeadline(t)
}
func (s brokenSocket) WriteToUDP(b []byte, to *net.UDPAddr) (int, error) {
	if s.sendErr != nil {
		return 0, s.sendErr
	}
	if s.short {
		return len(b) - 1, nil
	}
	return s.udpSocket.WriteToUDP(b, to)
}

func TestBrowseSendFailuresAndCleanup(t *testing.T) {
	for _, mode := range []string{"send", "deadline", "short", "read"} {
		t.Run(mode, func(t *testing.T) {
			m, _ := loopbackNetwork(t)
			original := m.openSender
			var opened udpSocket
			want := errors.New("injected send error")
			m.openSender = func(i lanInterface) (udpSocket, error) {
				c, err := original(i)
				if err != nil {
					return nil, err
				}
				opened = c
				s := brokenSocket{udpSocket: c}
				switch mode {
				case "send":
					s.sendErr = want
				case "deadline":
					s.deadlineErr = want
				case "short":
					s.short = true
				case "read":
					s.readErr = want
				}
				return s, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := m.Browse(ctx, func(wire.Peer) {}); err == nil {
				t.Fatal("socket failure reported as empty LAN")
			}
			if _, err := opened.WriteToUDP([]byte("x"), m.group); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("query socket leaked: %v", err)
			}
		})
	}
}

func TestBrowseRetriesAndNonceValidation(t *testing.T) {
	m, r := loopbackNetwork(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 65536)
		_ = r.SetReadDeadline(time.Now().Add(2 * time.Second))
		for i := 0; i < 3; i++ {
			n, src, err := r.ReadFromUDP(buf)
			if err != nil {
				return
			}
			q, err := decode(buf[:n])
			if err != nil {
				return
			}
			if i < 2 {
				continue
			} // Only the 900 ms retransmission gets a response.
			bad, _ := encode(announcement(testPeer().ID, testPeer()))
			_, _ = r.WriteToUDP(bad, src)
			_, _ = r.WriteToUDP(make([]byte, MaxDatagram+1), src)
			good, _ := encode(announcement(q.Query, testPeer()))
			_, _ = r.WriteToUDP(good, src)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 1100*time.Millisecond)
	defer cancel()
	count := 0
	if err := m.Browse(ctx, func(wire.Peer) { count++ }); err != nil {
		t.Fatal(err)
	}
	<-done
	if count != 1 {
		t.Fatalf("retries/nonce filtering failed: callbacks=%d", count)
	}
}

func TestResponderRejectsMalformedQueriesAndSurvives(t *testing.T) {
	m, _ := loopbackNetwork(t)
	ad, err := m.Announce(testPeer())
	if err != nil {
		t.Fatal(err)
	}
	defer ad.Close()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	good, _ := encode(queryPacket(testQuery))
	for _, b := range [][]byte{[]byte("{"), make([]byte, MaxDatagram+1), []byte(`{"protocol":"other"}`), good} {
		if _, err := c.WriteToUDP(b, m.group); err != nil {
			t.Fatal(err)
		}
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 65536)
	n, src, err := c.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	p, err := decode(buf[:n])
	if _, ok := p.peer(testQuery, src); err != nil || !ok {
		t.Fatalf("valid query lost after malformed packets: %v %v", p, err)
	}
	// Close is safe even when several owners race during shutdown.
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if err := ad.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

func TestAnnouncePartialInterfaceSetupFailure(t *testing.T) {
	m, _ := loopbackNetwork(t)
	m.events = make(chan Event, 16)
	m.ifaces = append(m.ifaces, lanInterface{iface: net.Interface{Name: "broken"}})
	original := m.openReceiver
	m.openReceiver = func(i lanInterface) (udpSocket, error) {
		if i.iface.Name == "broken" {
			return nil, errors.New("unavailable interface")
		}
		return original(i)
	}
	ad, err := m.Announce(testPeer())
	if err != nil {
		t.Fatal(err)
	}
	defer ad.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	count := 0
	if err := m.Browse(ctx, func(wire.Peer) { count++ }); err != nil || count != 1 {
		t.Fatalf("surviving responder unusable: %v callbacks=%d", err, count)
	}
}

func TestConfigureClosedSocket(t *testing.T) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if err := configure(c, net.Interface{Index: 1}); err == nil {
		t.Fatal("ignored multicast socket option failure")
	}
}

func TestPartialInterfaceFailureAndLimiter(t *testing.T) {
	m, _ := loopbackNetwork(t)
	ad, err := m.Announce(testPeer())
	if err != nil {
		t.Fatal(err)
	}
	defer ad.Close()
	m.ifaces = append(m.ifaces, lanInterface{iface: net.Interface{Name: "broken"}})
	original := m.openSender
	m.openSender = func(i lanInterface) (udpSocket, error) {
		if i.iface.Name == "broken" {
			return nil, errors.New("unavailable adapter")
		}
		return original(i)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	count := 0
	if err := m.Browse(ctx, func(wire.Peer) { count++ }); err != nil || count != 1 {
		t.Fatalf("partial interface failure: %v count=%d", err, count)
	}
	var l replyLimiter
	now := time.Now()
	for range 128 {
		if !l.allow(now) {
			t.Fatal("initial burst")
		}
	}
	if l.allow(now) {
		t.Fatal("unbounded responses")
	}
	if !l.allow(now.Add(time.Second)) {
		t.Fatal("rate limiter prevented later retry")
	}
}
