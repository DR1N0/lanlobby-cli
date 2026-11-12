package local

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// On Windows this is a logical namespace, not a directory. No runtime files are
// needed: Windows releases named-pipe handles when the owning process exits.
func DefaultDir() string { return "lanlobby" }

func pipeEndpoint(dir, room, user string) (string, *windows.SID, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", nil, fmt.Errorf("current Windows user: %w", err)
	}
	sid := u.User.Sid
	name := key(sid.String(), key(dir, key(room, user)))
	return `\\.\pipe\lanlobby-` + name, sid, nil
}

func pipeConfig(sid *windows.SID) *winio.PipeConfig {
	// Set the owner explicitly so clients can reject another user's pipe before
	// sending anything. The protected DACL grants only this user access.
	// go-winio also sets FILE_PIPE_REJECT_REMOTE_CLIENTS.
	return &winio.PipeConfig{
		SecurityDescriptor: "O:" + sid.String() + "D:P(A;;GA;;;" + sid.String() + ")",
		InputBufferSize:    64 * 1024,
		OutputBufferSize:   64 * 1024,
	}
}

type Membership struct {
	Listener net.Listener
	once     sync.Once
}

func Acquire(dir, room, user string) (*Membership, error) {
	path, sid, err := pipeEndpoint(dir, room, user)
	if err != nil {
		return nil, err
	}
	// ListenPipe keeps an exclusive first-instance handle for its entire life;
	// a contender cannot create another listener or unlink the owner's pipe.
	listener, err := winio.ListenPipe(path, pipeConfig(sid))
	if err != nil {
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_PIPE_BUSY) {
			return nil, ErrJoined
		}
		// Some Windows collision paths return ACCESS_DENIED, which can also
		// indicate a genuine permission failure. Keep that error rather than
		// incorrectly classifying every permission failure as an active join.
		return nil, fmt.Errorf("open local membership pipe (check for an existing join or access restrictions): %w", err)
	}
	return &Membership{Listener: listener}, nil
}

func (m *Membership) Close() {
	m.once.Do(func() { _ = m.Listener.Close() })
}

func Dial(ctx context.Context, dir, room, user string) (net.Conn, error) {
	path, sid, err := pipeEndpoint(dir, room, user)
	if err != nil {
		return nil, err
	}
	conn, err := winio.DialPipeContext(ctx, path)
	if err != nil {
		return nil, err
	}
	if err := verifyPipeOwner(conn, sid); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func verifyPipeOwner(conn net.Conn, expected *windows.SID) error {
	fd, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return errors.New("local pipe does not expose an ownership handle")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(fd.Fd()), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("verify local pipe owner: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("read local pipe owner: %w", err)
	}
	if owner == nil || !owner.Equals(expected) {
		return errors.New("local pipe is not owned by the current Windows user")
	}
	return nil
}
