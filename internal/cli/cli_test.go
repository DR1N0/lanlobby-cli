package cli

import (
	"bytes"
	"context"
	"flag"
	"strings"
	"testing"

	"github.com/DR1N0/lanlobby-cli/internal/version"
	"github.com/DR1N0/lanlobby-cli/internal/wire"
)

func TestParseUserCommands(t *testing.T) {
	cases := []struct {
		args []string
		want Command
	}{
		{[]string{"join", "room1", "--as", "agent-a"}, Command{Kind: "join", Room: "room1", User: "agent-a"}},
		{[]string{"send", "room1", "--as", "agent-a", "hello"}, Command{Kind: "send", Room: "room1", User: "agent-a", Body: "hello"}},
		{[]string{"list", "room"}, Command{Kind: "list-room"}},
		{[]string{"list", "room", "--debug"}, Command{Kind: "list-room", Debug: true}},
		{[]string{"list", "user", "--room=r", "--debug"}, Command{Kind: "list-user", Room: "r", Debug: true}},
		{[]string{"join", "r", "--as=u", "--debug"}, Command{Kind: "join", Room: "r", User: "u", Debug: true}},
		{[]string{"version"}, Command{Kind: "version"}},
		{[]string{"version", "--short"}, Command{Kind: "version", Short: true}},
		{[]string{"list", "user", "--room=room1"}, Command{Kind: "list-user", Room: "room1"}},
		{[]string{"join", "r", "--as=u", "--interface=en0"}, Command{Kind: "join", Room: "r", User: "u", Interface: "en0"}},
		{[]string{"send", "r", "--as", "u", "--", "--a-message"}, Command{Kind: "send", Room: "r", User: "u", Body: "--a-message"}},
	}
	for _, tc := range cases {
		got, err := Parse(tc.args)
		if err != nil || got != tc.want {
			t.Errorf("%q: got %+v, %v, want %+v", tc.args, got, err, tc.want)
		}
	}
	for _, args := range [][]string{{"join"}, {"join", "r"}, {"join", "r", "--as", "u", "extra"}, {"send", "r", "--as", "u"}, {"send", "r", "--as", "u", "two", "messages"}, {"list", "user"}, {"list", "rooms"}, {"list", "room", "--room=r"}, {"version", "extra"}, {"version", "--unknown"}, {"version", "--debug"}, {"send", "r", "--as=u", "--debug", "hello"}, {"wat"}} {
		if _, err := Parse(args); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
	if _, err := Parse([]string{"--help"}); err != flag.ErrHelp {
		t.Fatal(err)
	}
}

func TestVersionCommand(t *testing.T) {
	v := version.Get()
	for _, short := range []bool{false, true} {
		var out, diagnostics bytes.Buffer
		args := []string{"version"}
		if short {
			args = append(args, "--short")
		}
		if code := Run(context.Background(), args, Options{Output: &out, Diagnostics: &diagnostics}); code != 0 || diagnostics.Len() != 0 {
			t.Fatalf("version exit %d: %s", code, diagnostics.String())
		}
		if short {
			if got := out.String(); got != v.GitVersion+"\n" {
				t.Fatalf("short version: %q", got)
			}
		} else {
			for _, field := range []string{"lanlobby " + v.GitVersion, "GitCommit: " + v.GitCommit, "GitTreeState: " + v.GitTreeState, "BuildDate: " + v.BuildDate, "GoVersion: " + v.GoVersion, "Platform: " + v.Platform} {
				if !strings.Contains(out.String(), field+"\n") {
					t.Errorf("missing %q in %q", field, out.String())
				}
			}
		}
	}
}

func TestExitCodesAndTextLists(t *testing.T) {
	var out, diagnostics bytes.Buffer
	opts := Options{Output: &out, Diagnostics: &diagnostics, RuntimeDir: "/no-such-lanlobby-test-dir"}
	if code := Run(context.Background(), []string{"--help"}, opts); code != 0 || !strings.Contains(out.String(), "lanlobby join") || diagnostics.Len() != 0 {
		t.Fatal("help output")
	}
	out.Reset()
	if code := Run(context.Background(), []string{"send", "r", "--as", "u", "hello"}, opts); code != 1 || out.Len() != 0 || !strings.Contains(diagnostics.String(), "no active local join") {
		t.Fatal("send implicitly joined or wrong output")
	}
	diagnostics.Reset()
	if code := Run(context.Background(), []string{"list", "user"}, opts); code != 2 || diagnostics.Len() == 0 {
		t.Fatal("bad usage exit")
	}
	peers := []wire.Peer{{Room: "z", User: "b"}, {Room: "a", User: "a"}, {Room: "z", User: "c"}}
	out.Reset()
	if err := list(&out, "list-room", peers); err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(out.String()); strings.Join(got, " ") != "ROOM USERS a 1 z 2" {
		t.Fatalf("bad room list %q", out.String())
	}
	out.Reset()
	if err := list(&out, "list-user", nil); err != nil || strings.Join(strings.Fields(out.String()), " ") != "USER ADDRESS" {
		t.Fatal("empty list")
	}
}
