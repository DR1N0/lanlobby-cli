package room

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Client is one connection to a room node.
type Client struct {
	conn    net.Conn
	enc     *bufio.Writer
	dec     *reader
	mu      sync.Mutex
	seq     int64
	Room    string
	User    string
	Session string
}

// Dial connects to a node and completes the hello/welcome handshake. An empty
// user asks the node to assign a unique default name.
func Dial(ctx context.Context, addr, room, user string) (*Client, error) {
	return dial(ctx, addr, room, user, false)
}

// DialEphemeral joins without announcing membership, so a one-shot send does
// not emit join/leave notices to the room.
func DialEphemeral(ctx context.Context, addr, room, user string) (*Client, error) {
	return dial(ctx, addr, room, user, true)
}

func dial(ctx context.Context, addr, room, user string, ephemeral bool) (*Client, error) {
	d := net.Dialer{Timeout: DialTimeout}
	nc, err := d.DialContext(ctx, "tcp4", addr)
	if err != nil {
		return nil, fmt.Errorf("connect %s: %w", addr, err)
	}
	c := &Client{conn: nc, enc: bufio.NewWriter(nc), dec: newReader(nc)}
	hello := Frame{Type: TypeHello, Room: room, User: SanitizeName(user), Host: hostName(), Role: RoleClient, Ephemeral: ephemeral}
	if err := c.send(hello); err != nil {
		_ = nc.Close()
		return nil, err
	}
	_ = nc.SetReadDeadline(time.Now().Add(HandshakeTimeout))
	f, err := c.dec.Read()
	if err != nil {
		_ = nc.Close()
		return nil, fmt.Errorf("handshake with %s failed: %w (both ends must run the same build and the same room)", addr, err)
	}
	_ = nc.SetReadDeadline(time.Time{})
	if f.Type == TypeError {
		_ = nc.Close()
		return nil, errors.New(f.Error)
	}
	if f.Type != TypeWelcome {
		_ = nc.Close()
		return nil, fmt.Errorf("unexpected handshake response %q", f.Type)
	}
	c.Room, c.User, c.Session = room, f.User, f.Session
	return c, nil
}

func (c *Client) send(f Frame) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := writeFrame(c.enc, f); err != nil {
		return err
	}
	return c.enc.Flush()
}

// Send delivers one chat message and returns its sequence number.
func (c *Client) Send(body string) (int64, error) {
	if err := validateBody(body); err != nil {
		return 0, err
	}
	c.mu.Lock()
	c.seq++
	seq := c.seq
	c.mu.Unlock()
	if err := c.send(Frame{Type: TypeMessage, Room: c.Room, User: c.User, Origin: c.Session, Seq: seq, Body: body}); err != nil {
		return 0, err
	}
	return seq, nil
}

// WaitAck waits for the node to acknowledge a message.
func (c *Client) WaitAck(seq int64, timeout time.Duration) error {
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	defer func() { _ = c.conn.SetReadDeadline(time.Time{}) }()
	for {
		f, err := c.dec.Read()
		if err != nil {
			return err
		}
		switch f.Type {
		case TypeAck:
			if f.Seq == seq {
				return nil
			}
		case TypeError:
			return errors.New(f.Error)
		case TypePing:
			if err := c.send(Frame{Type: TypePong}); err != nil {
				return err
			}
		}
	}
}

// Receive reads frames until the connection ends or ctx is cancelled, invoking
// on for messages and notices. It answers node pings.
func (c *Client) Receive(ctx context.Context, on func(Frame)) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		f, err := c.dec.Read()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
			}
			return err
		}
		switch f.Type {
		case TypeMessage:
			on(f)
		case TypeNotice:
			on(f)
		case TypePing:
			if err := c.send(Frame{Type: TypePong}); err != nil {
				return err
			}
		case TypeError:
			return errors.New(f.Error)
		case TypeLeave:
			return nil
		}
	}
}

// Members requests the current room roster.
func (c *Client) Members() ([]Member, error) {
	if err := c.send(Frame{Type: TypeMembers}); err != nil {
		return nil, err
	}
	_ = c.conn.SetReadDeadline(time.Now().Add(RequestTimeout))
	defer func() { _ = c.conn.SetReadDeadline(time.Time{}) }()
	for {
		f, err := c.dec.Read()
		if err != nil {
			return nil, err
		}
		switch f.Type {
		case TypeMembers:
			return f.Members, nil
		case TypePing:
			if err := c.send(Frame{Type: TypePong}); err != nil {
				return nil, err
			}
		case TypeError:
			return nil, errors.New(f.Error)
		}
	}
}

// Close says goodbye and drops the connection.
func (c *Client) Close() error {
	_ = c.send(Frame{Type: TypeLeave})
	return c.conn.Close()
}
