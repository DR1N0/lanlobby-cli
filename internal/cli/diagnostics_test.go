package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DR1N0/lanlobby-cli/internal/discovery"
	"github.com/DR1N0/lanlobby-cli/internal/wire"
)

type emptyDiscovery struct{}

type joinDiscovery struct {
	emptyDiscovery
	announced, withdrawn chan struct{}
}
type closeAdvertisement func() error

func (f closeAdvertisement) Close() error { return f() }
func (n joinDiscovery) Announce(wire.Peer) (io.Closer, error) {
	close(n.announced)
	return closeAdvertisement(func() error { close(n.withdrawn); return nil }), nil
}

func TestJoinCancellationWithBlockedStderr(t *testing.T) {
	out := &blockedDiagnostics{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(out.release)
	n := joinDiscovery{announced: make(chan struct{}), withdrawn: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	base := "/tmp"
	if runtime.GOOS == "windows" {
		base = ""
	}
	dir, err := os.MkdirTemp(base, "ll-cli-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	done := make(chan int, 1)
	go func() {
		done <- Run(ctx, []string{"join", "r", "--as=u", "--debug"}, Options{Diagnostics: out, Network: n, RuntimeDir: dir, DiscoveryWindow: time.Millisecond})
	}()
	select {
	case <-n.announced:
	case <-time.After(5 * time.Second):
		t.Fatal("join never announced")
	}
	select {
	case <-out.entered:
	case <-time.After(time.Second):
		t.Fatal("status never reached stderr")
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("cancel exit=%d", code)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked stderr prevented join cancellation")
	}
	select {
	case <-n.withdrawn:
	default:
		t.Fatal("join returned without withdrawing discovery")
	}
}

func (emptyDiscovery) Announce(wire.Peer) (io.Closer, error) { panic("list must not join") }
func (emptyDiscovery) Browse(ctx context.Context, emit func(wire.Peer)) error {
	<-ctx.Done()
	return nil
}

func TestDebugOnlyOnStderr(t *testing.T) {
	for _, debug := range []bool{false, true} {
		var out, stderr bytes.Buffer
		args := []string{"list", "room"}
		if debug {
			args = append(args, "--debug")
		}
		code := Run(context.Background(), args, Options{Output: &out, Diagnostics: &stderr, Network: emptyDiscovery{}, DiscoveryWindow: time.Millisecond})
		if code != 0 || strings.Join(strings.Fields(out.String()), " ") != "ROOM USERS" {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), stderr.String())
		}
		if debug {
			if !strings.Contains(stderr.String(), "candidates=0 live=0") || !strings.Contains(stderr.String(), "[debug]") {
				t.Fatalf("missing diagnostics: %s", stderr.String())
			}
		} else if stderr.Len() != 0 {
			t.Fatalf("default diagnostics: %q", stderr.String())
		}
	}
}

func TestDiagnosticFilteringAndEscaping(t *testing.T) {
	for _, debug := range []bool{false, true} {
		var out bytes.Buffer
		events, finish := startDiagnostics(&out, debug)
		events <- discovery.Event{Level: "debug", Message: "detail"}
		events <- discovery.Event{Level: "warning", Message: "bad\n\x1b[31m"}
		_, _ = diagnosticWriter{events: events}.Write([]byte("joined r as u; Ctrl-C to leave\n"))
		finish()
		got := out.String()
		if strings.Contains(got, "detail") != debug || !strings.Contains(got, "[warning]") || strings.Contains(got, "\x1b") || strings.Count(got, "\n") != 2+boolInt(debug) {
			t.Fatalf("unsafe/filtering output %q", got)
		}
		if !strings.Contains(got, "joined r as u; Ctrl-C to leave\n") {
			t.Fatalf("changed status format: %q", got)
		}
	}
}
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

type blockedDiagnostics struct {
	entered, release chan struct{}
	once             sync.Once
}

func (w *blockedDiagnostics) Write(b []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(b), nil
}

func TestBlockedDiagnosticsDoNotBlockProducersOrFinish(t *testing.T) {
	out := &blockedDiagnostics{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(out.release)
	events, finish := startDiagnostics(out, true)
	writer := diagnosticWriter{events: events}
	_, _ = writer.Write([]byte("status\n"))
	select {
	case <-out.entered:
	case <-time.After(time.Second):
		t.Fatal("writer not started")
	}
	done := make(chan struct{})
	go func() {
		for range 10000 {
			_, _ = writer.Write([]byte("status\n"))
		}
		finish()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stderr held producer or shutdown")
	}
}
