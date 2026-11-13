package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"reflect"
	"strings"
	"testing"

	"github.com/DR1N0/lanlobby-cli/internal/version"
)

func TestParseCommands(t *testing.T) {
	cases := []struct {
		args []string
		want Command
	}{
		{
			[]string{"serve", "dev", "--as", "mac", "--listen", "0.0.0.0:7717", "--peer", "192.168.1.7:7717", "--debug"},
			Command{Kind: "serve", Room: "dev", User: "mac", Listen: "0.0.0.0:7717", Peers: []string{"192.168.1.7:7717"}, Debug: true},
		},
		{[]string{"join", "dev", "--as", "alice"}, Command{Kind: "join", Room: "dev", User: "alice"}},
		{[]string{"join", "dev", "--peer", "192.168.1.6:7717"}, Command{Kind: "join", Room: "dev", Peers: []string{"192.168.1.6:7717"}}},
		{[]string{"say", "dev", "--as", "bob", "hello world"}, Command{Kind: "say", Room: "dev", User: "bob", Body: "hello world"}},
		{[]string{"join", "dev", "--read-only", "--json"}, Command{Kind: "join", Room: "dev", ReadOnly: true, JSON: true}},
		{[]string{"join", "dev", "--no-self"}, Command{Kind: "join", Room: "dev", NoSelf: true}},
		{[]string{"peers", "dev", "--peer", "x:1"}, Command{Kind: "peers", Room: "dev", Peers: []string{"x:1"}}},
		{[]string{"version", "--short"}, Command{Kind: "version", Short: true}},
		{[]string{"serve", "dev", "--peer", "a:1", "--peer", "b:2"}, Command{Kind: "serve", Room: "dev", Peers: []string{"a:1", "b:2"}}},
	}
	for _, tc := range cases {
		got, err := Parse(tc.args)
		if err != nil {
			t.Errorf("%q: %v", tc.args, err)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: got %+v, want %+v", tc.args, got, tc.want)
		}
	}
}

func TestParseRejects(t *testing.T) {
	for _, args := range [][]string{
		{"serve"},
		{"serve", "--room", "dev"},
		{"serve", "a", "b"},
		{"serve", "dev", "--no-self"},
		{"join"},
		{"join", "a", "b"},
		{"say", "dev"},
		{"say", "dev", "a", "b"},
		{"peers"},
		{"wat"},
		{"join", "dev", "--nope"},
		{"version", "extra"},
	} {
		if _, err := Parse(args); err == nil {
			t.Errorf("accepted %q", args)
		}
	}
	if _, err := Parse([]string{"--help"}); err != flag.ErrHelp {
		t.Fatalf("help: %v", err)
	}
}

func TestVersionOutput(t *testing.T) {
	var out bytes.Buffer
	if code := Run(context.Background(), []string{"version"}, Options{Output: &out}); code != 0 {
		t.Fatalf("version exit %d", code)
	}
	for _, want := range []string{"lanlobby", "commit", "state", "built", "go ", "platform"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("version output missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if code := Run(context.Background(), []string{"version", "--json"}, Options{Output: &out}); code != 0 {
		t.Fatalf("version --json exit %d", code)
	}
	var m map[string]string
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("invalid json %q: %v", out.String(), err)
	}
	for _, k := range []string{"version", "commit", "state", "built", "go", "platform"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("json missing %q: %v", k, m)
		}
	}

	out.Reset()
	if code := Run(context.Background(), []string{"version", "--short"}, Options{Output: &out}); code != 0 {
		t.Fatalf("version --short exit %d", code)
	}
	if got := out.String(); got != version.Get().GitVersion+"\n" {
		t.Fatalf("short version: %q", got)
	}
}
