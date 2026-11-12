//go:build darwin || linux

package local

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

func DefaultDir() string { return fmt.Sprintf("/tmp/lanlobby-%d", os.Getuid()) }

func directory(dir string, create bool) (int, error) {
	if create {
		if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return -1, err
		}
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	var st unix.Stat_t
	if err = unix.Fstat(fd, &st); err == nil && (st.Uid != uint32(os.Getuid()) || st.Mode&0777 != 0700) {
		err = errors.New("runtime directory must be owned by this user with mode 0700")
	}
	if err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func SocketPath(dir, room, user string) (string, error) {
	fd, err := directory(dir, false)
	if err != nil {
		return "", err
	}
	defer unix.Close(fd)
	name := key(room, user) + ".sock"
	var st unix.Stat_t
	if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return "", err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFSOCK || st.Uid != uint32(os.Getuid()) || st.Mode&0077 != 0 {
		return "", errors.New("unsafe local session socket")
	}
	return filepath.Join(dir, name), nil
}

func Dial(ctx context.Context, dir, room, user string) (net.Conn, error) {
	path, err := SocketPath(dir, room, user)
	if err != nil {
		return nil, err
	}
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}

type Membership struct {
	Listener net.Listener
	lock     *os.File
	dirfd    int
	name     string
	ino      uint64
	dev      uint64
	once     sync.Once
}

func Acquire(dir, room, user string) (_ *Membership, result error) {
	fd, err := directory(dir, true)
	if err != nil {
		return nil, err
	}
	m := &Membership{dirfd: fd, name: key(room, user) + ".sock"}
	defer func() {
		if result != nil {
			m.Close()
		}
	}()
	lockfd, err := unix.Openat(fd, key(room, user)+".lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	m.lock = os.NewFile(uintptr(lockfd), "membership-lock")
	var lockStat unix.Stat_t
	if err := unix.Fstat(lockfd, &lockStat); err != nil {
		return nil, err
	}
	if lockStat.Mode&unix.S_IFMT != unix.S_IFREG || lockStat.Uid != uint32(os.Getuid()) || lockStat.Nlink != 1 || lockStat.Mode&0077 != 0 {
		return nil, errors.New("unsafe membership lock")
	}
	if err := unix.Flock(lockfd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrJoined
		}
		return nil, err
	}
	var st unix.Stat_t
	if err := unix.Fstatat(fd, m.name, &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		if st.Mode&unix.S_IFMT != unix.S_IFSOCK || st.Uid != uint32(os.Getuid()) {
			return nil, errors.New("refusing to remove non-socket runtime entry")
		}
		if err := unix.Unlinkat(fd, m.name, 0); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, unix.ENOENT) {
		return nil, err
	}
	path := filepath.Join(dir, m.name)
	if len(path) >= 104 {
		return nil, errors.New("runtime socket path is too long")
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	listener.SetUnlinkOnClose(false)
	m.Listener = listener
	if err := unix.Fstatat(fd, m.name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, err
	}
	m.ino, m.dev = st.Ino, uint64(st.Dev)
	if err := os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Membership) Close() {
	m.once.Do(func() {
		if m.Listener != nil {
			_ = m.Listener.Close()
		}
		// Only remove the socket created by this lock holder.
		if m.ino != 0 {
			var st unix.Stat_t
			if unix.Fstatat(m.dirfd, m.name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Ino == m.ino && uint64(st.Dev) == m.dev {
				_ = unix.Unlinkat(m.dirfd, m.name, 0)
			}
		}
		if m.lock != nil {
			_ = m.lock.Close()
		}
		_ = unix.Close(m.dirfd)
	})
}
