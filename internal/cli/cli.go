// Package cli parses and runs the lanlobby command set.
package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/DR1N0/lanlobby-cli/internal/room"
	"github.com/DR1N0/lanlobby-cli/internal/version"
)

const Usage = `Usage:
  lanlobby serve ROOM [--as NAME] [--listen ADDR] [--peer ADDR ...] [--advertise IP] [--debug]
  lanlobby join ROOM [--as NAME] [--peer ADDR] [--read-only] [--json]
  lanlobby say ROOM MESSAGE [--as NAME] [--peer ADDR]
  lanlobby peers ROOM [--peer ADDR] [--json]
  lanlobby version [--short] [--json]

serve runs a foreground room node and prints the join commands for this and
other machines and streams the room to stdout as it arrives. join, say and
peers connect to a node: the local one by default (127.0.0.1:7717), or
--peer host:port for a remote one. join --read-only watches without reading
stdin.

Nodes talk to each other only when started with --peer; there is no multicast
discovery. Chat messages are relayed by the node and never stored.
Ctrl-C leaves.
`

type Command struct {
	Kind      string
	Room      string
	User      string
	Body      string
	Peers     []string
	Listen    string
	Advertise string
	Debug     bool
	JSON      bool
	Short     bool
	ReadOnly  bool
}

// Parse accepts flags in any position, collects positional arguments, and
// validates the shape of each subcommand.
func Parse(args []string) (Command, error) {
	var c Command
	if len(args) == 0 {
		return c, flag.ErrHelp
	}
	if args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		return c, flag.ErrHelp
	}
	c.Kind = args[0]
	allowed := map[string]bool{}
	switch c.Kind {
	case "serve":
		allowed = map[string]bool{"as": true, "peer": true, "listen": true, "advertise": true, "debug": true}
	case "join":
		allowed = map[string]bool{"as": true, "peer": true, "read-only": true, "json": true}
	case "say":
		allowed = map[string]bool{"as": true, "peer": true}
	case "peers":
		allowed = map[string]bool{"peer": true, "json": true}
	case "version":
		allowed = map[string]bool{"short": true, "json": true}
	default:
		return c, fmt.Errorf("unknown command %q", c.Kind)
	}
	var pos []string

	for i := 1; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			pos = append(pos, a)
			continue
		}
		name := strings.TrimPrefix(a, "--")
		var val string
		hasVal := false
		if k := strings.IndexByte(name, '='); k >= 0 {
			val, name, hasVal = name[k+1:], name[:k], true
		}
		next := func() (string, error) {
			if hasVal {
				return val, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("--%s requires a value", name)
			}
			i++
			return args[i], nil
		}
		if !allowed[name] {
			return c, fmt.Errorf("--%s is not valid for %s", name, c.Kind)
		}
		switch name {
		case "as":
			v, err := next()
			if err != nil {
				return c, err
			}
			c.User = v
		case "peer":
			v, err := next()
			if err != nil {
				return c, err
			}
			c.Peers = append(c.Peers, v)
		case "listen":
			v, err := next()
			if err != nil {
				return c, err
			}
			c.Listen = v
		case "advertise":
			v, err := next()
			if err != nil {
				return c, err
			}
			c.Advertise = v
		case "debug":
			c.Debug = true
		case "json":
			c.JSON = true
		case "read-only":
			c.ReadOnly = true
		case "short":
			c.Short = true
		case "help", "h":
			return c, flag.ErrHelp
		default:
			return c, fmt.Errorf("unknown flag --%s", name)
		}
	}

	switch c.Kind {
	case "version":
		if len(pos) != 0 {
			return c, errors.New("version takes no arguments")
		}
	case "serve":
		if len(pos) != 1 {
			return c, errors.New("serve requires ROOM")
		}
		c.Room = pos[0]
	case "join":
		if len(pos) != 1 {
			return c, errors.New("join requires ROOM")
		}
		c.Room = pos[0]
	case "say":
		if len(pos) != 2 {
			return c, errors.New(`say requires ROOM and one quoted MESSAGE: lanlobby say dev "hello"`)
		}
		c.Room, c.Body = pos[0], pos[1]
	case "peers":
		if len(pos) != 1 {
			return c, errors.New("peers requires ROOM")
		}
		c.Room = pos[0]
	}
	if c.Kind != "serve" && len(c.Peers) > 1 {
		return c, fmt.Errorf("%s accepts only one --peer", c.Kind)
	}
	return c, nil
}

type Options struct {
	Output      io.Writer
	Diagnostics io.Writer
	Input       io.Reader
}

// Run returns conventional exit codes: 0 success, 1 runtime error, 2 usage error.
func Run(ctx context.Context, args []string, opts Options) int {
	if opts.Output == nil {
		opts.Output = io.Discard
	}
	if opts.Diagnostics == nil {
		opts.Diagnostics = io.Discard
	}
	if opts.Input == nil {
		opts.Input = os.Stdin
	}
	c, err := Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		_, _ = io.WriteString(opts.Output, Usage)
		return 0
	}
	if err != nil {
		_, _ = fmt.Fprintf(opts.Diagnostics, "lanlobby: %s\n\n%s", room.SafeText(err.Error()), Usage)
		return 2
	}
	switch c.Kind {
	case "version":
		return runVersion(c, opts)
	case "serve":
		err = runServe(ctx, c, opts)
	case "join":
		err = runJoin(ctx, c, opts)
	case "say":
		err = runSay(ctx, c, opts)
	case "peers":
		err = runPeers(ctx, c, opts)
	}
	if err != nil {
		if ctx.Err() != nil {
			return 0
		}
		_, _ = fmt.Fprintf(opts.Diagnostics, "lanlobby: %s\n", room.SafeText(err.Error()))
		return 1
	}
	return 0
}

func runVersion(c Command, opts Options) int {
	v := version.Get()
	if c.Short {
		_, _ = fmt.Fprintln(opts.Output, room.SafeText(v.GitVersion))
		return 0
	}
	if c.JSON {
		enc := json.NewEncoder(opts.Output)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(map[string]string{
			"version":  v.GitVersion,
			"commit":   v.GitCommit,
			"state":    v.GitTreeState,
			"built":    v.BuildDate,
			"go":       v.GoVersion,
			"platform": v.Platform,
		})
		return 0
	}
	w := tabwriter.NewWriter(opts.Output, 0, 0, 2, ' ', 0)
	for _, row := range [][2]string{
		{"lanlobby", v.GitVersion},
		{"  commit", v.GitCommit},
		{"  state", v.GitTreeState},
		{"  built", v.BuildDate},
		{"  go", v.GoVersion},
		{"  platform", v.Platform},
	} {
		_, _ = fmt.Fprintf(w, "%s\t%s\n", row[0], room.SafeText(row[1]))
	}
	_ = w.Flush()
	return 0
}

func nameOrDefault(u string) string {
	if s := room.SanitizeName(u); s != "" {
		return s
	}
	return room.DefaultName()
}

func peerAddr(c Command) (string, error) {
	addr := "127.0.0.1:7717"
	if len(c.Peers) == 1 {
		addr = c.Peers[0]
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return "", fmt.Errorf("invalid --peer %q: %w", addr, err)
	}
	return addr, nil
}

func runServe(ctx context.Context, c Command, opts Options) error {
	name := nameOrDefault(c.User)
	srv := &room.Server{
		Room:        c.Room,
		Name:        name,
		Listen:      c.Listen,
		Peers:       c.Peers,
		Debug:       c.Debug,
		Diagnostics: opts.Diagnostics,
		Output:      opts.Output,
	}
	srv.OnReady = func(addr string) {
		host, port := splitHostPort(addr)
		host = strings.Trim(host, "[]")
		ip := c.Advertise
		if ip == "" {
			// A wildcard listen has no single address to advertise; use the
			// first usable LAN address. A specific listen address speaks for
			// itself (e.g. 127.0.0.1 for same-machine tests).
			if host == "0.0.0.0" || host == "::" || host == "" {
				ip = room.LocalIP()
			} else {
				ip = host
			}
		}
		printServeBanner(opts.Output, c.Room, name, ip, port)
	}
	return srv.Run(ctx)
}

func printServeBanner(out io.Writer, roomName, name, ip, port string) {
	_, _ = fmt.Fprintf(out, "lanlobby serve: room %q as %q\n", roomName, name)
	if ip == "" {
		_, _ = fmt.Fprintf(out, "  listening on port %s\n", port)
	} else {
		_, _ = fmt.Fprintf(out, "  listening %s:%s\n", ip, port)
	}
	if port == "7717" {
		_, _ = fmt.Fprintf(out, "  from this machine:    lanlobby join %s --as alice\n", roomName)
	} else {
		_, _ = fmt.Fprintf(out, "  from this machine:    lanlobby join %s --as alice --peer 127.0.0.1:%s\n", roomName, port)
	}
	if ip != "" && !isLoopback(ip) {
		_, _ = fmt.Fprintf(out, "  from another machine: lanlobby join %s --as alice --peer %s:%s\n", roomName, ip, port)
		_, _ = fmt.Fprintf(out, "  add another node:     lanlobby serve %s --as <name> --peer %s:%s\n", roomName, ip, port)
	}
	_, _ = fmt.Fprintln(out)
}

func isLoopback(ip string) bool {
	return strings.HasPrefix(ip, "127.") || ip == "::1"
}

func splitHostPort(addr string) (string, string) {
	i := strings.LastIndexByte(addr, ':')
	if i < 0 {
		return addr, ""
	}
	return addr[:i], addr[i+1:]
}

func runJoin(ctx context.Context, c Command, opts Options) error {
	addr, err := peerAddr(c)
	if err != nil {
		return err
	}
	client, err := room.Dial(ctx, addr, c.Room, c.User)
	if err != nil {
		return err
	}
	defer client.Close()
	if !c.JSON {
		_, _ = fmt.Fprintf(opts.Output, "joined %q as %q via %s; Ctrl-C to leave\n", client.Room, client.User, addr)
	}

	joinCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !c.ReadOnly {
		go func() {
			defer cancel()
			sc := bufio.NewScanner(opts.Input)
			for sc.Scan() {
				line := strings.TrimSpace(sc.Text())
				if line == "" {
					continue
				}
				if _, err := client.Send(line); err != nil {
					return
				}
				if !c.JSON {
					_, _ = fmt.Fprintf(opts.Output, "%s: %s\n", room.SafeText(client.User), room.SafeText(line))
				}
			}
		}()
	}

	err = client.Receive(joinCtx, func(f room.Frame) {
		if c.JSON {
			_, _ = fmt.Fprintln(opts.Output, frameJSON(f))
			return
		}
		switch f.Type {
		case room.TypeMessage:
			_, _ = fmt.Fprintf(opts.Output, "%s: %s\n", room.SafeText(f.User), room.SafeText(f.Body))
		case room.TypeNotice:
			_, _ = fmt.Fprintf(opts.Output, "* %s\n", room.SafeText(f.Body))
		}
	})
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func runSay(ctx context.Context, c Command, opts Options) error {
	addr, err := peerAddr(c)
	if err != nil {
		return err
	}
	client, err := room.DialEphemeral(ctx, addr, c.Room, c.User)
	if err != nil {
		return err
	}
	defer client.Close()
	seq, err := client.Send(c.Body)
	if err != nil {
		return err
	}
	if err := client.WaitAck(seq, room.RequestTimeout); err != nil {
		return fmt.Errorf("message not acknowledged: %w", err)
	}
	if !c.JSON {
		_, _ = fmt.Fprintf(opts.Output, "sent as %q to %s\n", client.User, addr)
	}
	return nil
}

func runPeers(ctx context.Context, c Command, opts Options) error {
	addr, err := peerAddr(c)
	if err != nil {
		return err
	}
	client, err := room.Dial(ctx, addr, c.Room, "")
	if err != nil {
		return err
	}
	defer client.Close()
	members, err := client.Members()
	if err != nil {
		return err
	}
	if c.JSON {
		enc := json.NewEncoder(opts.Output)
		enc.SetEscapeHTML(false)
		return enc.Encode(map[string]any{"room": c.Room, "members": members})
	}
	w := tabwriter.NewWriter(opts.Output, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "USER\tROLE\tSESSION")
	for _, m := range members {
		role := "client"
		if m.Peer {
			role = "node"
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", room.SafeText(m.User), role, m.Session)
	}
	return w.Flush()
}

type jsonFrame struct {
	Type   string `json:"type"`
	Room   string `json:"room,omitempty"`
	User   string `json:"user,omitempty"`
	Origin string `json:"origin,omitempty"`
	Seq    int64  `json:"seq,omitempty"`
	Body   string `json:"body,omitempty"`
}

func frameJSON(f room.Frame) string {
	b, _ := json.Marshal(jsonFrame{
		Type:   f.Type,
		Room:   f.Room,
		User:   f.User,
		Origin: f.Origin,
		Seq:    f.Seq,
		Body:   f.Body,
	})
	return string(b)
}
