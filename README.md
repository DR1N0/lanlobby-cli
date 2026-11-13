# lanlobby-cli

Live LAN chat for agents and terminals. **Pure CLI. Direct TCP. No central server. No history.**

Each machine runs one foreground node (`serve`). Nodes connect to each other only when given an explicit `--peer`; clients (`join`, `say`, `peers`) talk to a node over TCP. A node streams the room to its terminal, so `serve` alone is enough to watch a conversation. No multicast discovery, leader, account, database, TUI, or Web UI.

## Build

Requires Go **1.26+** to build. Each platform gets a standalone binary; no Go installation is needed to run it.

With Make and a POSIX-compatible shell, build the initial three-platform matrix:

```bash
make help
make test
make build
```

| Platform | Artifact |
| --- | --- |
| macOS / arm64 | `bin/darwin/lanlobby` |
| Linux / amd64 | `bin/linux/lanlobby` |
| Windows / amd64 | `bin/windows/lanlobby.exe` |

For example, run `./bin/darwin/lanlobby --help` on Apple Silicon. On Windows, run `.\\lanlobby.exe --help` after copying the Windows artifact. `make clean` removes `bin/`.

To build just for the current machine, without Make:

```bash
go build -trimpath -o bin/lanlobby ./cmd/lanlobby
./bin/lanlobby --help
```

Use `./bin/lanlobby` directly, or add this checkout's `bin` to your current shell's path:

```bash
export PATH="$PWD/bin:$PATH"
```

Alternatively, `go install ./cmd/lanlobby` installs into `GOBIN`, or `$(go env GOPATH)/bin` when `GOBIN` is unset. No system service needs to be installed or started.

## Version

```bash
lanlobby version
# lanlobby   v0.0.1
#   commit   unknown
#   state    unknown
#   built    unknown
#   go       go1.27.3
#   platform windows/amd64

lanlobby version --short
# v0.0.1

lanlobby version --json
# {"built":"unknown","commit":"unknown","go":"go1.27.3","platform":"windows/amd64","state":"unknown","version":"v0.0.1"}
```

`make build` runs `hack/generate-ldflags.sh` and injects the version tag, Git commit, worktree state, and UTC build date with Go `-ldflags -X`. The full command also prints the Go version and target platform. It is local-only: no LAN discovery or update-server requests.

The initial `BUILD_TAG` is `v0.0.1`. Override it for later builds with `make build BUILD_TAG=v0.0.2` or `BUILD_TAG=v0.0.2 make build`. All three artifacts in one build share the same metadata. This embeds a version; it does not create Git tags or publish a release.

A plain `go build`/`go install` uses the source default version and reports uninjected commit/state/build-date fields as `unknown`.

## Usage

### Serve a room

Every machine that takes part runs one foreground node:

```bash
lanlobby serve dev --as mac --listen 0.0.0.0:7717
```

It prints how to join from this machine and from others:

```text
lanlobby serve: room "dev" as "mac"
  listening 192.168.1.6:7717
  from this machine:    lanlobby join dev --as alice
  from another machine: lanlobby join dev --as alice --peer 192.168.1.6:7717
  add another node:     lanlobby serve dev --as <name> --peer 192.168.1.6:7717
```

`--listen 0.0.0.0:7717` is the default. A node holds the room, relays messages between its clients and peer nodes, and keeps no history.

### Join, send, watch

```bash
# interactive member: reads stdin, prints live messages
lanlobby join dev --as alice

# watch only: no stdin, just the live stream (add --json for one object per line)
lanlobby join dev --read-only --json

# one-shot message from a script or agent; acknowledged, no join/leave notice
lanlobby say dev --as agent-a "build finished"

# roster: clients and peer nodes
lanlobby peers dev
```

`join`, `say` and `peers` connect to `127.0.0.1:7717` by default. Use `--peer host:port` for a node on another machine:

```bash
lanlobby join dev --as alice --peer 192.168.1.6:7717
```

### Connect two machines

Start a node on each machine and point one at the other:

```bash
# machine A
lanlobby serve dev --as mac --listen 0.0.0.0:7717
# machine B
lanlobby serve dev --as win --listen 0.0.0.0:7717 --peer 192.168.1.6:7717
```

Nodes reconnect automatically when a peer restarts. A second node is optional: run `serve` on one machine and `join ... --peer` from the other.

### When a machine cannot accept inbound connections

A managed Mac (or any host whose firewall cannot be changed) cannot be dialed.
It can still take part: run its node on loopback and dial out to the reachable
machine. Clients on it talk to the local node over loopback, so no inbound
permission is needed.

```bash
# Reachable machine (e.g. Windows) is the node
lanlobby serve dev --as win --listen 0.0.0.0:7717

# Firewalled machine (e.g. a managed Mac): loopback only, dial out
lanlobby serve dev --as mac --listen 127.0.0.1:7717 --peer 192.168.1.7:7717

# On the Mac, clients use its local node
lanlobby join dev --as alice
lanlobby say dev "hello" --as bob
```

`join`, `say` and `peers` always default to `127.0.0.1:7717`. A machine that
runs no local node must pass `--peer <node-ip>:7717` on **every** client
invocation; a node that is only dialing out should listen on `127.0.0.1` so it
does not advertise an address that its firewall will drop.

### Names

`--as` is optional. Without it the node assigns a unique `<host>-<n>` name to each client and reports it. Give agents and humans distinct names when several sessions share a machine; two live sessions in one room may not share a name.

### Firewalls

Each node must accept inbound TCP on its port. Allow the program once rather than a port range:

```powershell
# Windows (admin PowerShell); adjust the path to the built binary
New-NetFirewallRule -DisplayName "lanlobby" -Direction Inbound -Program "C:\path\lanlobby.exe" -Action Allow -Profile Private
```

```bash
# macOS
sudo /usr/libexec/ApplicationFirewall/socketfilterfw --add /path/to/lanlobby
sudo /usr/libexec/ApplicationFirewall/socketfilterfw --unblockapp /path/to/lanlobby
```

There is no multicast or broadcast dependency: a plain TCP path between the machines is enough.

## Output and delivery

- No prompts, ANSI colors, cursor movement, or full-screen interface.
- `serve` prints the room stream (`NAME: MESSAGE`) to stdout as messages arrive; `join` does the same and also reads stdin, one message per line. `join --read-only` skips stdin. `say` takes one quoted message and never prints the room. `--json` makes `join` emit one JSON object per line instead of `NAME: MESSAGE`.
- stdout is the message stream. Status, usage errors, and delivery errors go to stderr.
- Exit codes: `0` success/help/clean interruption, `1` runtime or delivery error, `2` invalid command or arguments.
- Names are 1–64 UTF-8 bytes without whitespace, control, or formatting characters. Messages are 1–8 KiB of UTF-8 text.
- A `say` succeeds only after the node acknowledges the message; the node relays it once to every other live client and peer node. There is no global ordering guarantee across nodes.
- Node-to-node connections reconnect automatically. `join` does not reconnect on its own: rerun it after its node restarts.

## No history

There is no persistence, replay, catch-up, offline queue, or automatic resend. A late joiner sees only new messages. Leaving and rejoining creates a fresh session and never restores old output.

While joined, there is a bounded in-memory queue for immediate display. The CLI itself does not write chat text to disk. Redirecting stdout or capturing terminal output is, of course, up to the caller. No local socket, pipe, or lock files are created: clients reach the node over TCP and the node keeps no state on disk.

## Network and trust boundary

- **Transport:** plain IPv4 TCP. Clients dial the node's listen address; peer nodes dial each other. Nothing is sent over UDP, so multicast, broadcast, IGMP, and AP-isolation behaviour cannot block the room.
- **Firewalls:** each node must accept inbound TCP on its port from the other machines. Prefer a program-specific inbound rule (see Firewalls) over disabling the firewall. Clients only need outbound TCP. Managed macOS hosts cannot change the application firewall from the command line ("Firewall settings cannot be modified from command line on managed Mac computers"); such a host cannot be a dialed node, but it can run a loopback node and dial out (see "When a machine cannot accept inbound connections").
- **Reachability direction:** a TCP connection carries both directions once established, so a one-way LAN restriction only requires choosing which side dials the other.
- No internet service is used at runtime.
- **Trusted LAN only: no encryption, passwords, cryptographic identities, or authenticated room membership.** Room names, usernames, and messages travel in plaintext. Anyone with network access can read or forge traffic; room names are not secrets.
- Duplicate live names in one room are rejected by the node. Names are per node, not globally consistent across a mesh.

## Development and verification

```bash
go fmt ./...
go vet ./...
go test ./...
go test -race ./...
```

The unit tests start real nodes on loopback and cover two-client relay, cross-node relay, peer reconnect, duplicate-name rejection, and frame bounds. They need no LAN.

### Two-machine smoke test

1. `make build` (or `go build`) and copy the artifact to both machines.
2. On A: `lanlobby serve dev --as mac --listen 0.0.0.0:7717`. On B: `lanlobby serve dev --as win --listen 0.0.0.0:7717 --peer <A-ip>:7717`.
3. Allow the program through each firewall (see Firewalls). `lanlobby peers dev` on either side should list the other node.
4. `lanlobby join dev --as alice` on A and `lanlobby say dev "hello" --as bob` on B: the message must appear on A. Repeat in the other direction.
5. `lanlobby join dev --read-only --json` should produce one JSON object per line with `user`, `body`, and `seq`.
6. Restart A's node; B should reconnect within a few seconds. Restart `join` after its node restarts.
