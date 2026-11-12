package discovery

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/DR1N0/lanlobby-cli/internal/wire"
)

func TestSnapshotDiagnosticsDistinguishTCPFailure(t *testing.T) {
	p, stop := probeServer(t)
	stop()
	events := make(chan Event, 16)
	n := WithDiagnostics(staticNetwork{peers: []wire.Peer{p}}, events)
	peers, err := Live(context.Background(), n, "", time.Millisecond)
	if err != nil || len(peers) != 0 {
		t.Fatalf("stale snapshot: %v %v", peers, err)
	}
	var output strings.Builder
	for len(events) > 0 {
		output.WriteString((<-events).Message)
		output.WriteByte('\n')
	}
	if !strings.Contains(output.String(), "TCP verification failed") || !strings.Contains(output.String(), "candidates=1 live=0") {
		t.Fatalf("ambiguous diagnostics: %s", output.String())
	}
}

func TestReportNeverBlocks(t *testing.T) {
	events := make(chan Event, 1)
	done := make(chan struct{})
	go func() {
		for range 10000 {
			report(events, "debug", "packet %d", 1)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("full diagnostic queue stalled networking")
	}
}
