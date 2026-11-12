// Package integration drives the real binary. Opt in because mDNS requires an
// actual multicast-capable LAN; ordinary go test ./... stays network-independent.
package integration

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DR1N0/lanlobby-cli/internal/wire"
)

type join struct {
	cmd      *exec.Cmd
	messages chan string
	ready    chan struct{}
	done     chan struct{}
	err      error
	mu       sync.Mutex
	log      bytes.Buffer
}

func launch(t *testing.T, binary, room, user string) *join {
	t.Helper()
	j := &join{cmd: exec.Command(binary, "join", room, "--as", user), messages: make(chan string, 32), ready: make(chan struct{}, 1), done: make(chan struct{})}
	out, err := j.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	diag, err := j.cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := j.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		scanner := bufio.NewScanner(out)
		for scanner.Scan() {
			j.messages <- scanner.Text()
		}
	}()
	go func() {
		scanner := bufio.NewScanner(diag)
		for scanner.Scan() {
			line := scanner.Text()
			j.mu.Lock()
			fmt.Fprintln(&j.log, line)
			j.mu.Unlock()
			if strings.HasPrefix(line, "joined ") {
				select {
				case j.ready <- struct{}{}:
				default:
				}
			}
		}
	}()
	go func() { j.err = j.cmd.Wait(); close(j.done) }()
	t.Cleanup(func() {
		select {
		case <-j.done:
			return
		default:
		}
		_ = j.cmd.Process.Kill()
		<-j.done
	})
	select {
	case <-j.ready:
		return j
	case <-j.done:
		j.mu.Lock()
		s := j.log.String()
		j.mu.Unlock()
		t.Fatalf("join %s exited: %v %s", user, j.err, s)
	case <-time.After(5 * time.Second):
		t.Fatalf("join %s not ready", user)
	}
	return nil
}
func (j *join) stop(t *testing.T, kill bool) {
	t.Helper()
	if kill {
		_ = j.cmd.Process.Kill()
	} else {
		if err := j.cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatalf("interrupt join: %v", err)
		}
	}
	select {
	case <-j.done:
		if !kill && j.err != nil {
			t.Fatalf("Ctrl-C exit: %v", j.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("join failed to leave")
	}
}
func (j *join) expect(t *testing.T, want string) {
	t.Helper()
	select {
	case got := <-j.messages:
		if got != want {
			t.Fatalf("message got %q want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		j.mu.Lock()
		s := j.log.String()
		j.mu.Unlock()
		t.Fatalf("no message %q; diagnostics: %s", want, s)
	}
}
func (j *join) empty(t *testing.T) {
	t.Helper()
	select {
	case got := <-j.messages:
		t.Fatalf("unexpected history/message: %q", got)
	case <-time.After(100 * time.Millisecond):
	}
}
func command(t *testing.T, binary string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
	return string(out), err
}

func TestRealLANCLI(t *testing.T) {
	if os.Getenv("LANLOBBY_LAN_TEST") != "1" {
		t.Skip("set LANLOBBY_LAN_TEST=1 to exercise real multicast and the CLI")
	}
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Dir(filepath.Dir(file))
	name := "lanlobby"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)
	build := exec.Command("go", "build", "-o", binary, "./cmd/lanlobby")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	id, err := wire.NewID()
	if err != nil {
		t.Fatal(err)
	}
	room := "smoke-" + id[:8]
	if out, err := command(t, binary, "send", room, "--as", "agent-a", "not joined"); err == nil {
		t.Fatalf("unjoined user sent a message: %s", out)
	}
	a := launch(t, binary, room, "agent-a")
	b := launch(t, binary, room, "agent-b")
	other := launch(t, binary, room+"-other", "agent-c")
	users, err := command(t, binary, "list", "user", "--room="+room)
	if err != nil || !strings.Contains(users, "agent-a") || !strings.Contains(users, "agent-b") {
		t.Fatalf("LAN discovery failed: %v\n%s", err, users)
	}
	t.Logf("live users:\n%s", users)
	for _, tc := range []struct{ user, body string }{{"agent-a", "hello"}, {"agent-b", "reply"}} {
		if out, err := command(t, binary, "send", room, "--as", tc.user, tc.body); err != nil || out != "" {
			t.Fatalf("send: %v %s", err, out)
		}
		a.expect(t, tc.user+": "+tc.body)
		b.expect(t, tc.user+": "+tc.body)
		other.empty(t)
	}
	late := launch(t, binary, room, "late")
	late.empty(t)
	rooms, err := command(t, binary, "list", "room")
	if err != nil || !strings.Contains(rooms, room) {
		t.Fatalf("room missing: %v %s", err, rooms)
	}
	t.Logf("live rooms:\n%s", rooms)
	if out, err := command(t, binary, "join", room, "--as", "agent-a"); err == nil {
		t.Fatalf("duplicate joined: %s", out)
	}
	// Windows supports real console Ctrl-C, but Go cannot send os.Interrupt
	// to another Windows process. Exercise forced exit there, explicitly.
	forcedExit := runtime.GOOS == "windows"
	if forcedExit {
		t.Log("Windows: using forced termination; console Ctrl-C needs native manual verification")
	}
	b.stop(t, forcedExit)
	if out, err := command(t, binary, "send", room, "--as", "agent-b", "after leave"); err == nil {
		t.Fatalf("sent after leaving: %s", out)
	}
	users, err = command(t, binary, "list", "user", "--room="+room)
	if err != nil || strings.Contains(users, "agent-b") {
		t.Fatalf("departed user remains: %v %s", err, users)
	}
	b = launch(t, binary, room, "agent-b")
	b.empty(t)
	late.stop(t, true)
	if out, err := command(t, binary, "send", room, "--as", "late", "after kill"); err == nil {
		t.Fatalf("sent after SIGKILL: %s", out)
	}
	late = launch(t, binary, room, "late")
	late.empty(t)
	if out, err := command(t, binary, "send", room, "--as", "agent-a", "fresh"); err != nil {
		t.Fatalf("send after restarts: %v %s", err, out)
	}
	a.expect(t, "agent-a: fresh")
	b.expect(t, "agent-a: fresh")
	late.expect(t, "agent-a: fresh")
	other.empty(t)
	a.stop(t, forcedExit)
	b.stop(t, forcedExit)
	late.stop(t, forcedExit)
	other.stop(t, forcedExit)
	rooms, err = command(t, binary, "list", "room")
	if err != nil || strings.Contains(rooms, room) {
		t.Fatalf("empty room remains: %v %s", err, rooms)
	}
	t.Log("PASS: discovery, two-way TCP, plain stream, room isolation, duplicate join, termination/rejoin, and no history")
}
