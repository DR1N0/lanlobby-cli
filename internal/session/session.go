// Package session runs a room membership for exactly one foreground process.
package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/DR1N0/lanlobby-cli/internal/discovery"
	"github.com/DR1N0/lanlobby-cli/internal/local"
	"github.com/DR1N0/lanlobby-cli/internal/wire"
)

const (
	queueSize      = 64
	maxConnections = 32
)

type Options struct {
	Room        string
	User        string
	Network     discovery.Network
	Output      io.Writer
	Diagnostics io.Writer
	RuntimeDir  string
	// DiscoveryWindow and Ready are useful for deterministic process tests.
	DiscoveryWindow time.Duration
	Ready           func(wire.Peer)
}

type message struct{ user, body string }

type membership struct {
	ctx         context.Context
	cancel      context.CancelFunc
	self        wire.Peer
	network     discovery.Network
	window      time.Duration
	mu          sync.Mutex
	active      bool
	connections map[net.Conn]struct{}
	incoming    chan message
	limit       chan struct{}
	wg          sync.WaitGroup
	fatal       chan error
}

func Run(parent context.Context, opts Options) error {
	if err := wire.Identifier(opts.Room); err != nil {
		return fmt.Errorf("room: %w", err)
	}
	if err := wire.Identifier(opts.User); err != nil {
		return fmt.Errorf("user: %w", err)
	}
	if opts.Network == nil {
		return errors.New("missing LAN discovery")
	}
	if opts.Output == nil {
		opts.Output = io.Discard
	}
	if opts.Diagnostics == nil {
		opts.Diagnostics = io.Discard
	}
	if opts.RuntimeDir == "" {
		opts.RuntimeDir = local.DefaultDir()
	}
	if opts.DiscoveryWindow <= 0 {
		opts.DiscoveryWindow = discovery.Window
	}
	owner, err := local.Acquire(opts.RuntimeDir, opts.Room, opts.User)
	if err != nil {
		return fmt.Errorf("join %s as %s: %w", opts.Room, opts.User, err)
	}
	defer owner.Close()
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	id, err := wire.NewID()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	s := &membership{
		ctx: ctx, cancel: cancel,
		self:    wire.Peer{ID: id, Room: opts.Room, User: opts.User, Port: listener.Addr().(*net.TCPAddr).Port},
		network: opts.Network, window: opts.DiscoveryWindow, active: true,
		connections: make(map[net.Conn]struct{}), incoming: make(chan message, queueSize),
		limit: make(chan struct{}, maxConnections), fatal: make(chan error, 1),
	}
	defer func() {
		// Close the sending gate before cancellation or releasing ownership.
		s.mu.Lock()
		s.active = false
		cancel()
		_ = listener.Close()
		_ = owner.Listener.Close()
		for conn := range s.connections {
			_ = conn.Close()
		}
		s.mu.Unlock()
		s.wg.Wait()
	}()
	s.wg.Add(2)
	go s.serve(listener, false)
	go s.serve(owner.Listener, true)
	ad, err := opts.Network.Announce(s.self)
	if err != nil {
		return err
	}
	var discoveryErrors <-chan error
	if source, ok := ad.(interface{ Errors() <-chan error }); ok {
		discoveryErrors = source.Errors()
	}
	// Withdrawal happens only after the send gate has closed.
	defer func() {
		s.mu.Lock()
		s.active = false
		s.mu.Unlock()
		cancel()
		_ = ad.Close()
	}()
	outputError := make(chan error, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case m := <-s.incoming:
				if ctx.Err() != nil {
					return
				}
				if _, err := fmt.Fprintf(opts.Output, "%s: %s\n", wire.SafeText(m.user), wire.SafeText(m.body)); err != nil {
					outputError <- err
					return
				}
			}
		}
	}()
	s.wg.Add(1)
	go s.checkConflicts()
	if _, err := fmt.Fprintf(opts.Diagnostics, "joined %s as %s; Ctrl-C to leave\n", opts.Room, opts.User); err != nil {
		return err
	}
	if opts.Ready != nil {
		opts.Ready(s.self)
	}
	select {
	case <-parent.Done():
		return nil
	case err := <-s.fatal:
		return err
	case err := <-discoveryErrors:
		return fmt.Errorf("LAN discovery unavailable: %w", err)
	case err := <-outputError:
		return fmt.Errorf("write message stream: %w", err)
	}
}

func (s *membership) fail(err error) {
	if s.ctx.Err() == nil {
		select {
		case s.fatal <- err:
		default:
		}
	}
}

func (s *membership) serve(listener net.Listener, ipc bool) {
	defer s.wg.Done()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if s.ctx.Err() == nil {
				s.fail(fmt.Errorf("session listener: %w", err))
			}
			return
		}
		select {
		case s.limit <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		s.mu.Lock()
		if !s.active || s.ctx.Err() != nil {
			s.mu.Unlock()
			<-s.limit
			_ = conn.Close()
			continue
		}
		s.connections[conn] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { _ = conn.Close(); s.mu.Lock(); delete(s.connections, conn); s.mu.Unlock(); <-s.limit }()
			deadline := wire.Timeout
			if ipc {
				deadline = 10 * time.Second
			}
			_ = conn.SetDeadline(time.Now().Add(deadline))
			f, err := wire.Read(conn)
			if err != nil {
				s.reply(conn, "ack", err)
				return
			}
			if ipc {
				err = s.localRequest(f)
			} else {
				err = s.remoteRequest(conn, f)
			}
			typ := "ack"
			if f.Type == "probe" {
				typ = "probe_ack"
			}
			s.reply(conn, typ, err)
		}()
	}
}

func (s *membership) reply(conn net.Conn, typ string, err error) {
	f := wire.Frame{Version: wire.Version, Type: typ, Room: s.self.Room, User: s.self.User, Session: s.self.ID, Port: s.self.Port}
	if err != nil {
		f.Error = err.Error()
	}
	_ = wire.Write(conn, f)
}

func (s *membership) remoteRequest(conn net.Conn, f wire.Frame) error {
	if f.Room != s.self.Room || f.Target != s.self.ID {
		return errors.New("room or target session mismatch")
	}
	s.mu.Lock()
	active := s.active && s.ctx.Err() == nil
	s.mu.Unlock()
	if !active {
		return errors.New("membership has left the room")
	}
	if f.Type == "probe" {
		return nil
	}
	if f.Type != "message" {
		return errors.New("unsupported TCP request")
	}
	if err := wire.Body(f.Body); err != nil {
		return err
	}
	addr, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return errors.New("invalid sender address")
	}
	sender := wire.Peer{ID: f.Session, Room: f.Room, User: f.User, Port: f.Port, Addresses: []string{addr.IP.String()}}
	// Only probe the observed source IP. This is a liveness check, NOT auth.
	if err := wire.Probe(s.ctx, sender); err != nil {
		return fmt.Errorf("sender is no longer joined: %w", err)
	}
	return s.enqueue(message{f.User, f.Body})
}

func (s *membership) localRequest(f wire.Frame) error {
	if f.Type != "send" || f.Room != s.self.Room || f.User != s.self.User {
		return errors.New("no matching local room/user membership")
	}
	if err := wire.Body(f.Body); err != nil {
		return err
	}
	s.mu.Lock()
	active := s.active && s.ctx.Err() == nil
	s.mu.Unlock()
	if !active {
		return errors.New("membership has left the room")
	}
	ctx, cancel := context.WithTimeout(s.ctx, 8*time.Second)
	defer cancel()
	peers, err := discovery.Live(ctx, s.network, s.self.Room, s.window)
	if err != nil {
		return fmt.Errorf("find recipients: %w", err)
	}
	if err := conflict(s.self, peers); err != nil {
		s.fail(err)
		return err
	}
	if err := s.enqueue(message{s.self.User, f.Body}); err != nil {
		return err
	}
	var wg sync.WaitGroup
	limit := make(chan struct{}, 16)
	var mu sync.Mutex
	var failures []string
	for _, peer := range peers {
		if peer.ID == s.self.ID {
			continue
		}
		select {
		case limit <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		}
		wg.Add(1)
		go func(p wire.Peer) {
			defer wg.Done()
			defer func() { <-limit }()
			f := wire.Frame{Version: wire.Version, Type: "message", Room: s.self.Room, User: s.self.User, Session: s.self.ID, Port: s.self.Port, Target: p.ID, Body: f.Body}
			_, err := wire.Contact(ctx, p, f, "ack")
			if err != nil {
				mu.Lock()
				failures = append(failures, wire.SafeText(p.User)+": "+wire.SafeText(err.Error()))
				mu.Unlock()
			}
		}(peer)
	}
	wg.Wait()
	if len(failures) > 0 {
		sort.Strings(failures)
		return fmt.Errorf("partial or uncertain delivery (not retried): %s", strings.Join(failures, "; "))
	}
	return nil
}

func (s *membership) enqueue(m message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active || s.ctx.Err() != nil {
		return errors.New("membership has left the room")
	}
	select {
	case s.incoming <- m:
		return nil
	default:
		return errors.New("live output queue is full; message was not accepted")
	}
}

func conflict(self wire.Peer, peers []wire.Peer) error {
	for _, p := range peers {
		if p.Room == self.Room && p.User == self.User && p.ID < self.ID {
			return fmt.Errorf("name %q is already joined to %q on another session; choose a different --as", self.User, self.Room)
		}
	}
	return nil
}

func (s *membership) checkConflicts() {
	defer s.wg.Done()
	for {
		peers, err := discovery.Live(s.ctx, s.network, s.self.Room, s.window)
		if s.ctx.Err() != nil {
			return
		}
		if err != nil {
			s.fail(err)
			return
		}
		if err := conflict(s.self, peers); err != nil {
			s.fail(err)
			return
		}
		timer := time.NewTimer(3 * time.Second)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Send never joins implicitly or falls back to direct network access.
func Send(ctx context.Context, dir, room, user, body string) error {
	if err := wire.Identifier(room); err != nil {
		return fmt.Errorf("room: %w", err)
	}
	if err := wire.Identifier(user); err != nil {
		return fmt.Errorf("user: %w", err)
	}
	if err := wire.Body(body); err != nil {
		return err
	}
	if dir == "" {
		dir = local.DefaultDir()
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var dialErr error
	response, err := wire.ExchangeWithDial(ctx, func(ctx context.Context) (net.Conn, error) {
		conn, err := local.Dial(ctx, dir, room, user)
		dialErr = err
		return conn, err
	}, wire.Frame{Version: wire.Version, Type: "send", Room: room, User: user, Body: body})
	if err != nil {
		if dialErr != nil {
			return fmt.Errorf("no active local join for %s as %s (run lanlobby join first): %w", room, user, dialErr)
		}
		return fmt.Errorf("local join unavailable or send outcome unknown: %w", err)
	}
	if response.Type != "ack" || response.Room != room || response.User != user || !wire.ValidID(response.Session) {
		return errors.New("invalid local session response")
	}
	if response.Error != "" {
		return errors.New(response.Error)
	}
	return nil
}
