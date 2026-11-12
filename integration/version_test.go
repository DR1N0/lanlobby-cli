package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Unlike the multicast tests, build/version verification needs no LAN access.
func TestLinkedVersion(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("version injection script requires a POSIX shell")
	}
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Dir(filepath.Dir(file))
	script := filepath.Join(root, "hack", "generate-ldflags.sh")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	flags := exec.CommandContext(ctx, sh, script)
	flags.Dir = t.TempDir() // The script must locate the repo independently of cwd.
	flags.Env = append(os.Environ(), "BUILD_TAG=v9.8.7-test")
	out, err := flags.Output()
	if err != nil {
		t.Fatalf("generate linker flags: %v", err)
	}
	name := "lanlobby"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)
	build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-ldflags", strings.TrimSpace(string(out)), "-o", binary, "./cmd/lanlobby")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build linked version: %v\n%s", err, output)
	}
	output, err := command(t, binary, "version", "--short")
	if err != nil || output != "v9.8.7-test\n" {
		t.Fatalf("linker override not reflected: %q %v", output, err)
	}
	output, err = command(t, binary, "version")
	if err != nil {
		t.Fatalf("version command: %v %s", err, output)
	}
	for _, field := range []string{"lanlobby v9.8.7-test\n", "GitCommit: ", "GitTreeState: ", "Platform: " + runtime.GOOS + "/" + runtime.GOARCH + "\n"} {
		if !strings.Contains(output, field) {
			t.Errorf("missing %q in %s", field, output)
		}
	}
	date := ""
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "BuildDate: ") {
			date = strings.TrimPrefix(line, "BuildDate: ")
		}
	}
	if _, err := time.Parse(time.RFC3339, date); err != nil {
		t.Fatalf("invalid injected UTC build date %q: %v", date, err)
	}
	for _, tc := range []struct {
		tag   string
		valid bool
	}{{"", true}, {"unsafe tag", false}} {
		cmd := exec.CommandContext(ctx, sh, script)
		cmd.Env = append(os.Environ(), "BUILD_TAG="+tc.tag)
		out, err := cmd.Output()
		if tc.valid {
			if err != nil || !strings.Contains(string(out), ".gitVersion=v0.0.1 ") {
				t.Fatalf("default build tag: %q %v", out, err)
			}
		} else if err == nil || len(out) != 0 {
			t.Fatalf("unsafe tag accepted: %q %v", out, err)
		}
	}
}
