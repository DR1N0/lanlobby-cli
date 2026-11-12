package room

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DR1N0/lanlobby-cli/internal/wire"
)

// Server is one foreground room node: it accepts peer nodes and local/remote
// clients, and relays messages between them. It holds no history.
type Server struct {
	Room        string
	Name        string
	Listen      string
	Peers       []string
	Debug       bool
	Diagnostics io.Writer
	// OnReady is called once the listen socket is bound, before accepting
	// connections. It is where a CLI prints the join commands.
	OnReady func(addr string)
	// Output, when set, receives the live room stream (user: body), so a
	// foreground node doubles as a tail without a second command.
	Output io.Writer

	ln        net.Listener
	mu        sync.Mutex
	conns     map[*conn]struct{}
	members   map[string]*Member
	seen      map[signal]struct{}
	seenOrder []signal
	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
	addr      string
	id        string
	noticeSeq atomic.Int64
}

type signal struct {
	origin string
	seq    int64
}

type conn struct {
	srv       *Server
	nc        net.Conn
	enc       *bufio.Writer
	dec       *Reader
	mu        sync.Mutex
	role      string
	user      string
	session   string
	peer      bool
	ephemeral bool
	lastSeen  atomic.Int64 // unix nanoseconds
	closed    chan struct{}
	once      sync.Once
}

// Run starts the node and blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	addr := s.Listen
	if addr == "" {
		addr = "0.0.0.0:7717"
	}
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	s.ln = ln
	s.addr = ln.Addr().String()
	if s.OnReady != nil {
		s.OnReady(s.addr)
	}
	s.done = make(chan struct{})
	s.conns = make(map[*conn]struct{})
	s.members = make(map[string]*Member)
	s.seen = make(map[signal]struct{})
	s.id, _ = wire.NewID()

	for _, p := range s.Peers {
		p := p
		s.wg.Add(1)
		go func() { defer s.wg.Done(); s.keepPeer(p) }()
	}
	s.wg.Add(1)
	go func() { defer s.wg.Done(); s.pingLoop() }()

	go func() {
		<-ctx.Done()
		s.Close()
	}()

	s.wg.Add(1)
	go func() { defer s.wg.Done(); s.acceptLoop() }()

	<-s.done
	s.wg.Wait()
	return nil
}

// Addr is the bound listen address (useful with :0).
func (s *Server) Addr() string { return s.addr }

// Close stops accepting and drops every connection.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		if s.ln != nil {
			_ = s.ln.Close()
		}
		s.mu.Lock()
		conns := make([]*conn, 0, len(s.conns))
		for c := range s.conns {
			conns = append(conns, c)
		}
		s.mu.Unlock()
		for _, c := range conns {
			c.close()
		}
		close(s.done)
	})
}

func (s *Server) logf(format string, args ...any) {
	if !s.Debug || s.Diagnostics == nil {
		return
	}
	fmt.Fprintf(s.Diagnostics, "lanlobby [debug]: "+format+"\n", args...)
}

func (s *Server) acceptLoop() {
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if err := s.handle(nc, false); err != nil {
				s.logf("connection %s closed: %v", nc.RemoteAddr(), err)
			}
		}()
	}
}

// keepPeer maintains an outbound node connection, reconnecting forever.
func (s *Server) keepPeer(addr string) {
	for {
		select {
		case <-s.done:
			return
		default:
		}
		nc, err := net.DialTimeout("tcp", addr, DialTimeout)
		if err == nil {
			err = s.handle(nc, true)
		}
		if err != nil {
			s.logf("peer %s: %v", addr, err)
		}
		select {
		case <-s.done:
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *Server) handle(nc net.Conn, outbound bool) error {
	c := &conn{
		srv:    s,
		nc:     nc,
		enc:    bufio.NewWriter(nc),
		dec:    NewReader(nc),
		closed: make(chan struct{}),
	}
	c.lastSeen.Store(time.Now().UnixNano())
	defer c.close()

	_ = nc.SetDeadline(time.Now().Add(HandshakeTimeout))
	if outbound {
		c.role = RolePeer
		c.peer = true
		if err := c.send(Frame{Type: TypeHello, Room: s.Room, User: s.Name, Host: hostName(), Role: RolePeer}); err != nil {
			return err
		}
		f, err := c.dec.Read()
		if err != nil {
			return fmt.Errorf("handshake: %w", err)
		}
		if f.Type == TypeError {
			return errors.New(f.Error)
		}
		if f.Type != TypeWelcome {
			return fmt.Errorf("unexpected handshake response %q", f.Type)
		}
		c.user = SanitizeName(f.User)
		if c.user == "" {
			c.user = SanitizeName(f.Session)
		}
		c.session = f.Session
	} else {
		hello, err := c.dec.Read()
		if err != nil {
			return fmt.Errorf("handshake: %w", err)
		}
		if err := s.welcome(c, hello); err != nil {
			_ = c.send(Frame{Type: TypeError, Error: err.Error()})
			return err
		}
	}
	_ = nc.SetDeadline(time.Time{})

	s.register(c)
	s.logf("%s joined as %q session=%s remote=%s", c.role, c.user, c.session, nc.RemoteAddr())
	defer s.unregister(c)

	return c.readLoop()
}

// welcome validates the hello and replies with the assigned session and name.
func (s *Server) welcome(c *conn, hello Frame) error {
	if hello.Type != TypeHello {
		return fmt.Errorf("expected hello, got %q", hello.Type)
	}
	if hello.Version != Version {
		return fmt.Errorf("unsupported protocol version %d", hello.Version)
	}
	if hello.Room != s.Room {
		return fmt.Errorf("this node serves room %q, not %q", s.Room, hello.Room)
	}
	switch hello.Role {
	case RolePeer, "":
		c.role = RolePeer
		c.peer = true
		c.user = SanitizeName(hello.User)
		if c.user == "" {
			c.user = SanitizeName(s.Name)
		}
	case RoleClient:
		c.role = RoleClient
	default:
		return fmt.Errorf("unknown role %q", hello.Role)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if c.role == RoleClient {
		c.ephemeral = hello.Ephemeral
		if hello.User == "" {
			c.user = s.newNameLocked(hello.Host)
		} else {
			c.user = SanitizeName(hello.User)
			for _, m := range s.members {
				if m.User == c.user {
					return fmt.Errorf("name %q is already in this room; choose another --as", c.user)
				}
			}
		}
	}
	id, err := wire.NewID()
	if err != nil {
		return err
	}
	c.session = id
	welcomeUser := c.user
	if c.peer {
		welcomeUser = s.Name
	}
	return c.send(Frame{Type: TypeWelcome, Room: s.Room, User: welcomeUser, Session: c.session})
}

// newNameLocked picks a unique name for a client that did not pass --as.
func (s *Server) newNameLocked(host string) string {
	base := SanitizeName(host)
	if base == "" {
		base = "guest"
	}
	used := make(map[string]bool, len(s.members))
	for _, m := range s.members {
		used[m.User] = true
	}
	if !used[base] {
		return base
	}
	for i := 2; ; i++ {
		n := fmt.Sprintf("%s-%d", base, i)
		if !used[n] {
			return n
		}
	}
}

func (s *Server) register(c *conn) {
	s.mu.Lock()
	s.conns[c] = struct{}{}
	s.members[c.session] = &Member{User: c.user, Session: c.session, Peer: c.peer}
	s.mu.Unlock()

	if c.ephemeral {
		return
	}
	what := c.user
	if c.peer {
		what = c.user + " (node)"
	}
	s.notice(c, what+" joined")
}

func (s *Server) unregister(c *conn) {
	s.mu.Lock()
	_, ok := s.conns[c]
	delete(s.conns, c)
	delete(s.members, c.session)
	s.mu.Unlock()
	if !ok {
		return
	}
	if c.ephemeral {
		return
	}
	what := c.user
	if c.peer {
		what = c.user + " (node)"
	}
	s.notice(c, what+" left")
}

func (c *conn) send(f Frame) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := Write(c.enc, f); err != nil {
		return err
	}
	return c.enc.Flush()
}

func (c *conn) close() {
	c.once.Do(func() {
		close(c.closed)
		_ = c.nc.Close()
	})
}

func (c *conn) readLoop() error {
	for {
		f, err := c.dec.Read()
		if err != nil {
			select {
			case <-c.closed:
				return nil
			default:
			}
			return err
		}
		c.lastSeen.Store(time.Now().UnixNano())
		if err := c.handleFrame(f); err != nil {
			return err
		}
	}
}

func (c *conn) handleFrame(f Frame) error {
	s := c.srv
	switch f.Type {
	case TypePing:
		return c.send(Frame{Type: TypePong})
	case TypePong:
		return nil
	case TypeLeave:
		return io.EOF
	case TypeMembers:
		return c.send(Frame{Type: TypeMembers, Members: s.snapshot()})
	case TypeMessage:
		if len(f.Body) == 0 || len(f.Body) > MaxBody {
			return nil
		}
		if err := wire.Body(f.Body); err != nil {
			return nil
		}
		if c.role == RoleClient {
			// A client may only speak as itself.
			f.User = c.user
			f.Origin = c.session
			if f.Seq <= 0 {
				f.Seq = time.Now().UnixNano()
			}
		}
		if f.Origin == "" {
			f.Origin = c.session
		}
		if f.Seq <= 0 {
			return nil
		}
		if !s.firstSeen(f.Origin, f.Seq) {
			return nil
		}
		if s.Output != nil {
			_, _ = fmt.Fprintf(s.Output, "%s: %s\n", wire.SafeText(f.User), wire.SafeText(f.Body))
		}
		s.broadcast(c, Frame{Type: TypeMessage, User: f.User, Origin: f.Origin, Seq: f.Seq, Body: f.Body})
		if c.role == RoleClient {
			return c.send(Frame{Type: TypeAck, Seq: f.Seq})
		}
		return nil
	case TypeNotice:
		if f.Origin == "" || f.Seq <= 0 {
			return nil
		}
		if !s.firstSeen(f.Origin, f.Seq) {
			return nil
		}
		s.broadcast(c, Frame{Type: TypeNotice, Origin: f.Origin, Seq: f.Seq, Body: f.Body})
		return nil
	case TypeAck, TypeWelcome, TypeError, TypeHello:
		return nil
	default:
		return fmt.Errorf("unsupported frame type %q", f.Type)
	}
}

// firstSeen reports whether (origin, seq) is new, keeping a bounded window.
func (s *Server) firstSeen(origin string, seq int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := signal{origin, seq}
	if _, ok := s.seen[k]; ok {
		return false
	}
	s.seen[k] = struct{}{}
	s.seenOrder = append(s.seenOrder, k)
	if len(s.seenOrder) > 8192 {
		delete(s.seen, s.seenOrder[0])
		s.seenOrder = s.seenOrder[1:]
	}
	return true
}

// notice emits a local room notice that peer nodes will relay, deduplicated by
// (origin node, seq) so a larger mesh never loops.
func (s *Server) notice(from *conn, body string) {
	f := Frame{Type: TypeNotice, Origin: s.id, Seq: s.noticeSeq.Add(1), Body: body}
	s.firstSeen(f.Origin, f.Seq)
	s.broadcast(from, f)
}

func (s *Server) broadcast(from *conn, f Frame) {
	s.mu.Lock()
	conns := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		if c != from {
			conns = append(conns, c)
		}
	}
	s.mu.Unlock()
	for _, c := range conns {
		if err := c.send(f); err != nil {
			c.close()
		}
	}
}

func (s *Server) snapshot() []Member {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Member, 0, len(s.members))
	for _, m := range s.members {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Peer != out[j].Peer {
			return out[i].Peer
		}
		return out[i].User < out[j].User
	})
	return out
}

func (s *Server) pingLoop() {
	t := time.NewTicker(PingEvery)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			s.mu.Lock()
			conns := make([]*conn, 0, len(s.conns))
			for c := range s.conns {
				conns = append(conns, c)
			}
			s.mu.Unlock()
			now := time.Now()
			for _, c := range conns {
				last := time.Unix(0, c.lastSeen.Load())
				if now.Sub(last) > IdleTimeout {
					c.close()
					continue
				}
				if err := c.send(Frame{Type: TypePing}); err != nil {
					c.close()
				}
			}
		}
	}
}

// hostName returns the short host name used for default member names.
func hostName() string {
	h, err := hostname()
	if err != nil || h == "" {
		return "guest"
	}
	for i := 0; i < len(h); i++ {
		if h[i] == '.' {
			return h[:i]
		}
	}
	return h
}
