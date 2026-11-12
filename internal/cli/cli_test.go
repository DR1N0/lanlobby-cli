package cli

import (
	"flag"
	"reflect"
	"testing"
)

func TestParseCommands(t *testing.T) {
	cases := []struct {
		args []string
		want Command
	}{
		{
			[]string{"serve", "--room", "dev", "--as", "mac", "--listen", "0.0.0.0:7717", "--peer", "192.168.1.7:7717", "--debug"},
			Command{Kind: "serve", Room: "dev", User: "mac", Listen: "0.0.0.0:7717", Peers: []string{"192.168.1.7:7717"}, Debug: true},
		},
		{[]string{"join", "dev", "--as", "alice"}, Command{Kind: "join", Room: "dev", User: "alice"}},
		{[]string{"join", "dev", "--peer", "192.168.1.6:7717"}, Command{Kind: "join", Room: "dev", Peers: []string{"192.168.1.6:7717"}}},
		{[]string{"say", "dev", "--as", "bob", "hello world"}, Command{Kind: "say", Room: "dev", User: "bob", Body: "hello world"}},
		{[]string{"tail", "dev", "--json"}, Command{Kind: "tail", Room: "dev", JSON: true}},
		{[]string{"peers", "dev", "--peer", "x:1"}, Command{Kind: "peers", Room: "dev", Peers: []string{"x:1"}}},
		{[]string{"version", "--short"}, Command{Kind: "version", Short: true}},
		{[]string{"serve", "--room=dev"}, Command{Kind: "serve", Room: "dev"}},
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
		{"serve", "--room"},
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
