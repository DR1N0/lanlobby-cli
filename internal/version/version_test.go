package version

import (
	"runtime"
	"testing"
)

func TestGet(t *testing.T) {
	oldVersion, oldCommit, oldState, oldDate := gitVersion, gitCommit, gitTreeState, buildDate
	t.Cleanup(func() {
		gitVersion, gitCommit, gitTreeState, buildDate = oldVersion, oldCommit, oldState, oldDate
	})
	gitVersion, gitCommit, gitTreeState, buildDate = "v0.0.1-test", "abc123", "dirty", "2026-11-12T00:00:00Z"
	got := Get()
	want := Info{
		GitVersion: "v0.0.1-test", GitCommit: "abc123", GitTreeState: "dirty", BuildDate: "2026-11-12T00:00:00Z",
		GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
