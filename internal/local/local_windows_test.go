package local

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/DR1N0/lanlobby-cli/internal/wire"
	"golang.org/x/sys/windows"
)

func TestWindowsPipeDescriptor(t *testing.T) {
	dir := t.TempDir()
	path, sid, err := pipeEndpoint(dir, "room", "user")
	if err != nil {
		t.Fatal(err)
	}
	other, _, err := pipeEndpoint(dir+"-other", "room", "user")
	if err != nil || path == other {
		t.Fatalf("namespace collision: %v", err)
	}
	sd, err := windows.SecurityDescriptorFromString(pipeConfig(sid).SecurityDescriptor)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(sid) {
		t.Fatalf("wrong owner: %v", err)
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("DACL is not protected: %v", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 1 {
		t.Fatalf("expected one user-only ACE: %v", err)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	allowed := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || !allowed.Equals(sid) {
		t.Fatal("pipe ACL grants access to another principal")
	}
}

func TestWindowsMembershipExchangeAndRejoin(t *testing.T) {
	dir := t.TempDir()
	m, err := Acquire(dir, "room", "user")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if contender, err := Acquire(dir, "room", "user"); err == nil {
		contender.Close()
		t.Fatal("duplicate acquired pipe")
	}
	other, err := Acquire(dir, "other-room", "user")
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := m.Listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		_, err = wire.Read(conn)
		if err == nil {
			err = wire.Write(conn, wire.Frame{Version: wire.Version, Type: "ack", Body: "hello"})
		}
		done <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	response, err := wire.ExchangeWithDial(ctx, func(ctx context.Context) (net.Conn, error) { return Dial(ctx, dir, "room", "user") }, wire.Frame{Version: wire.Version, Type: "send", Body: "hello"})
	if err != nil || response.Type != "ack" || response.Body != "hello" {
		t.Fatalf("pipe exchange: %+v %v", response, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	m.Close()
	m.Close()
	if conn, err := Dial(ctx, dir, "room", "user"); err == nil {
		conn.Close()
		t.Fatal("dial after leaving succeeded")
	}
	m, err = Acquire(dir, "room", "user")
	if err != nil {
		t.Fatal("rejoin:", err)
	}
	m.Close()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("runtime files created: %v %v", entries, err)
	}
}

func TestWindowsPipeOwnerMismatch(t *testing.T) {
	dir := t.TempDir()
	m, err := Acquire(dir, "room", "user")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := m.Listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := Dial(ctx, dir, "room", "user")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	select {
	case server := <-accepted:
		defer server.Close()
	case <-ctx.Done():
		t.Fatal("accept timeout")
	}
	wrong, err := windows.StringToSid("S-1-1-0")
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyPipeOwner(conn, wrong); err == nil {
		t.Fatal("mismatched owner accepted")
	}
}

func TestWindowsPipeCancellationAndClose(t *testing.T) {
	dir := t.TempDir()
	m, err := Acquire(dir, "room", "user")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	// No Accept yet: the exclusive initial handle exists, but client connection
	// must wait. Cancellation must interrupt that wait without creating a send.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if conn, err := Dial(ctx, dir, "room", "user"); err == nil {
		conn.Close()
		t.Fatal("dial without accept succeeded")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("dial did not honor cancellation: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		conn, err := m.Listener.Accept()
		if err == nil {
			conn.Close()
		}
		done <- err
	}()
	m.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accept succeeded after close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not interrupt accept")
	}
}

func TestWindowsConcurrentMembership(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	winners := make(chan *Membership, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if m, err := Acquire(dir, "room", "user"); err == nil {
				winners <- m
			}
		}()
	}
	wg.Wait()
	close(winners)
	n := 0
	for m := range winners {
		n++
		m.Close()
	}
	if n != 1 {
		t.Fatalf("got %d owners, want one", n)
	}
	m, err := Acquire(dir, "room", "user")
	if err != nil {
		t.Fatal("rejoin:", err)
	}
	m.Close()
}

func TestWindowsPipeProcessHelper(t *testing.T) {
	if os.Getenv("LANLOBBY_PIPE_HELPER") != "1" {
		return
	}
	m, err := Acquire(os.Getenv("LANLOBBY_PIPE_NAMESPACE"), "room", "user")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer m.Close()
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestWindowsPipeProcessKillRecovery(t *testing.T) {
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestWindowsPipeProcessHelper$")
	cmd.Env = append(os.Environ(), "LANLOBBY_PIPE_HELPER=1", "LANLOBBY_PIPE_NAMESPACE="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if line != "ready\n" {
			t.Fatalf("helper: %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("helper timeout")
	}
	if contender, err := Acquire(dir, "room", "user"); err == nil {
		contender.Close()
		t.Fatal("cross-process duplicate acquired")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	m, err := Acquire(dir, "room", "user")
	if err != nil {
		t.Fatal("rejoin after process termination:", err)
	}
	m.Close()
}
