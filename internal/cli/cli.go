// Package cli provides the text-only lanlobby commands.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/DR1N0/lanlobby-cli/internal/discovery"
	"github.com/DR1N0/lanlobby-cli/internal/session"
	"github.com/DR1N0/lanlobby-cli/internal/version"
	"github.com/DR1N0/lanlobby-cli/internal/wire"
)

const Usage = `Usage:
  lanlobby join ROOM --as NAME [--interface IFACE] [--debug]
  lanlobby send ROOM --as NAME "MESSAGE"
  lanlobby list room [--interface IFACE] [--debug]
  lanlobby list user --room=ROOM [--interface IFACE] [--debug]
  lanlobby version [--short]

join stays in the foreground and streams live messages to stdout.
send requires a matching local join in another terminal. Ctrl-C leaves.
LAN discovery uses UDP multicast; chat messages use direct TCP. No history.
--debug prints discovery and TCP verification diagnostics to stderr.
`

type Command struct {
	Kind      string
	Room      string
	User      string
	Body      string
	Interface string
	Short     bool
	Debug     bool
}

func Parse(args []string) (Command, error) {
	var c Command
	if len(args) == 0 || (len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help")) {
		return c, flag.ErrHelp
	}
	c.Kind = args[0]
	f := flag.NewFlagSet("lanlobby "+c.Kind, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	switch c.Kind {
	case "version":
		f.BoolVar(&c.Short, "short", false, "print only the version tag")
		if err := f.Parse(args[1:]); err != nil {
			return c, err
		}
		if f.NArg() != 0 {
			return c, errors.New("unexpected version arguments")
		}
	case "join", "send":
		if len(args) < 2 {
			return c, errors.New("missing ROOM")
		}
		c.Room = args[1]
		f.StringVar(&c.User, "as", "", "local member name")
		if c.Kind == "join" {
			f.StringVar(&c.Interface, "interface", "", "LAN interface")
			f.BoolVar(&c.Debug, "debug", false, "discovery diagnostics on stderr")
		}
		if err := f.Parse(args[2:]); err != nil {
			return c, err
		}
		if c.Kind == "send" {
			if f.NArg() != 1 {
				return c, errors.New("send requires one quoted MESSAGE after the flags")
			}
			c.Body = f.Arg(0)
			if err := wire.Body(c.Body); err != nil {
				return c, err
			}
		} else if f.NArg() != 0 {
			return c, errors.New("unexpected join arguments")
		}
		if err := wire.Identifier(c.User); err != nil {
			return c, fmt.Errorf("--as: %w", err)
		}
	case "list":
		if len(args) < 2 || (args[1] != "room" && args[1] != "user") {
			return c, errors.New("list requires room or user")
		}
		c.Kind = "list-" + args[1]
		if c.Kind == "list-user" {
			f.StringVar(&c.Room, "room", "", "room to list")
		}
		f.StringVar(&c.Interface, "interface", "", "LAN interface")
		f.BoolVar(&c.Debug, "debug", false, "discovery diagnostics on stderr")
		if err := f.Parse(args[2:]); err != nil {
			return c, err
		}
		if f.NArg() != 0 {
			return c, errors.New("unexpected list arguments")
		}
	default:
		return c, fmt.Errorf("unknown command %q", c.Kind)
	}
	if c.Kind != "list-room" && c.Kind != "version" {
		if err := wire.Identifier(c.Room); err != nil {
			return c, fmt.Errorf("room: %w", err)
		}
	}
	return c, nil
}

type Options struct {
	Output          io.Writer
	Diagnostics     io.Writer
	Network         discovery.Network
	RuntimeDir      string
	DiscoveryWindow time.Duration
}

// Run returns conventional exit codes: 0 success, 1 runtime error, 2 usage error.
func Run(ctx context.Context, args []string, opts Options) int {
	if opts.Output == nil {
		opts.Output = io.Discard
	}
	if opts.Diagnostics == nil {
		opts.Diagnostics = io.Discard
	}
	c, err := Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		_, _ = io.WriteString(opts.Output, Usage)
		return 0
	}
	if err != nil {
		_, _ = fmt.Fprintf(opts.Diagnostics, "lanlobby: %s\n\n%s", wire.SafeText(err.Error()), Usage)
		return 2
	}
	if c.Kind == "version" {
		v := version.Get()
		if c.Short {
			_, err = fmt.Fprintln(opts.Output, wire.SafeText(v.GitVersion))
		} else {
			_, err = fmt.Fprintf(opts.Output, "lanlobby %s\nGitCommit: %s\nGitTreeState: %s\nBuildDate: %s\nGoVersion: %s\nPlatform: %s\n",
				wire.SafeText(v.GitVersion), wire.SafeText(v.GitCommit), wire.SafeText(v.GitTreeState), wire.SafeText(v.BuildDate), v.GoVersion, v.Platform)
		}
	} else if c.Kind == "send" {
		err = session.Send(ctx, opts.RuntimeDir, c.Room, c.User, c.Body)
	} else {
		network := opts.Network
		// Status and discovery share one bounded, nonblocking stderr queue.
		events, finish := startDiagnostics(opts.Diagnostics, c.Debug)
		opts.Diagnostics = diagnosticWriter{events: events}
		defer finish()
		if network == nil {
			network, err = discovery.New(c.Interface, events)
		} else {
			network = discovery.WithDiagnostics(network, events)
		}
		if err == nil {
			if c.Kind == "join" {
				err = session.Run(ctx, session.Options{Room: c.Room, User: c.User, Network: network, Output: opts.Output, Diagnostics: opts.Diagnostics, RuntimeDir: opts.RuntimeDir, DiscoveryWindow: opts.DiscoveryWindow})
			} else {
				var peers []wire.Peer
				peers, err = discovery.Live(ctx, network, c.Room, opts.DiscoveryWindow)
				if err == nil {
					err = list(opts.Output, c.Kind, peers)
				}
			}
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return 0
		}
		_, _ = fmt.Fprintf(opts.Diagnostics, "lanlobby: %s\n", wire.SafeText(err.Error()))
		return 1
	}
	return 0
}

func list(out io.Writer, kind string, peers []wire.Peer) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if kind == "list-room" {
		if _, err := fmt.Fprintln(w, "ROOM\tUSERS"); err != nil {
			return err
		}
		counts := make(map[string]int)
		for _, p := range peers {
			counts[p.Room]++
		}
		rooms := make([]string, 0, len(counts))
		for room := range counts {
			rooms = append(rooms, room)
		}
		sort.Strings(rooms)
		for _, room := range rooms {
			if _, err := fmt.Fprintf(w, "%s\t%d\n", wire.SafeText(room), counts[room]); err != nil {
				return err
			}
		}
	} else {
		if _, err := fmt.Fprintln(w, "USER\tADDRESS"); err != nil {
			return err
		}
		for _, p := range peers {
			if _, err := fmt.Fprintf(w, "%s\t%s\n", wire.SafeText(p.User), discovery.Address(p)); err != nil {
				return err
			}
		}
	}
	return w.Flush()
}

// Diagnostic output is best effort, including status when stderr is blocked.
// A single worker owns the underlying writer; network and leave never wait on it.
type diagnosticWriter struct{ events chan<- discovery.Event }

func (w diagnosticWriter) Write(b []byte) (int, error) {
	select {
	case w.events <- discovery.Event{Level: "status", Message: string(b)}:
	default:
	}
	return len(b), nil
}

func startDiagnostics(out io.Writer, debug bool) (chan<- discovery.Event, func()) {
	events := make(chan discovery.Event, 128)
	stop, done := make(chan struct{}), make(chan struct{})
	write := func(e discovery.Event) {
		if e.Level == "status" {
			_, _ = io.WriteString(out, e.Message)
			return
		}
		if e.Level == "debug" && !debug {
			return
		}
		_, _ = fmt.Fprintf(out, "lanlobby [%s]: %s\n", wire.SafeText(e.Level), wire.SafeText(e.Message))
	}
	go func() {
		defer close(done)
		for {
			select {
			case e := <-events:
				write(e)
			case <-stop:
				for {
					select {
					case e := <-events:
						write(e)
					default:
						return
					}
				}
			}
		}
	}()
	return events, func() {
		close(stop)
		// A blocked terminal must not hold discovery sockets or prevent leave.
		// At most this one writer can remain blocked until the process exits.
		timer := time.NewTimer(100 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
		}
	}
}
