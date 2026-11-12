//go:build darwin || linux

package local

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func runtimeDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "lanlobby-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestOwnershipAndCleanup(t *testing.T) {
	dir := runtimeDir(t)
	a, err := Acquire(dir, "room1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := Acquire(dir, "room1", "alice"); !errors.Is(err, ErrJoined) {
		t.Fatalf("duplicate joined: %v", err)
	}
	if _, err := SocketPath(dir, "room1", "alice"); err != nil {
		t.Fatal("failed contender removed active socket:", err)
	}
	b, err := Acquire(dir, "room2", "alice")
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
	a.Close()
	if _, err := SocketPath(dir, "room1", "alice"); err == nil {
		t.Fatal("socket survived leave")
	}
	a, err = Acquire(dir, "room1", "alice")
	if err != nil {
		t.Fatal("cannot rejoin:", err)
	}
	a.Close()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		st, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(e.Name(), ".lock") || st.Size() != 0 {
			t.Fatalf("unexpected runtime data: %s", e.Name())
		}
	}
}

func TestUnsafeRuntimeEntries(t *testing.T) {
	dir := runtimeDir(t)
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(link, "r", "u"); err == nil {
		t.Fatal("symlink runtime directory accepted")
	}
	if err := os.Chmod(real, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(real, "r", "u"); err == nil {
		t.Fatal("shared runtime directory accepted")
	}
	file := filepath.Join(dir, key("r", "u")+".sock")
	if err := os.WriteFile(file, []byte("not ours to delete"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(dir, "r", "u"); err == nil {
		t.Fatal("non-socket overwritten")
	}
	if b, _ := os.ReadFile(file); string(b) != "not ours to delete" {
		t.Fatal("unrelated file changed")
	}
}

func TestStaleSocket(t *testing.T) {
	dir := runtimeDir(t)
	path := filepath.Join(dir, key("r", "u")+".sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close()
	m, err := Acquire(dir, "r", "u")
	if err != nil {
		t.Fatal("stale socket not reclaimed:", err)
	}
	m.Close()
}

func TestLockProcessHelper(t *testing.T) {
	if os.Getenv("LANLOBBY_LOCK_HELPER") != "1" {
		return
	}
	m, err := Acquire(os.Getenv("LANLOBBY_TEST_DIR"), "room1", "alice")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer m.Close()
	fmt.Println("ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestSIGKILLAndConcurrentRejoin(t *testing.T) {
	dir := runtimeDir(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestLockProcessHelper$")
	cmd.Env = append(os.Environ(), "LANLOBBY_LOCK_HELPER=1", "LANLOBBY_TEST_DIR="+dir)
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
			t.Fatalf("helper failed: %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("helper timeout")
	}
	if _, err := Acquire(dir, "room1", "alice"); !errors.Is(err, ErrJoined) {
		t.Fatalf("cross-process lock failed: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	// Keep the winner locked until every contender has attempted acquisition.
	var wg sync.WaitGroup
	winners := make(chan *Membership, 4)
	results := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := Acquire(dir, "room1", "alice")
			if err == nil {
				winners <- m
			}
			results <- err
		}()
	}
	wg.Wait()
	close(winners)
	close(results)
	n := 0
	for m := range winners {
		n++
		m.Close()
	}
	if n != 1 {
		t.Fatalf("expected one rejoin winner, got %d", n)
	}
	for err := range results {
		if err != nil && !errors.Is(err, ErrJoined) {
			t.Fatal(err)
		}
	}
	m, err := Acquire(dir, "room1", "alice")
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
}
