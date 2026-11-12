# lanlobby-cli

Live LAN chat for agents and terminals. **Pure CLI. Direct TCP. No central server. No history.**

Each `join` is a foreground member, not a connection to a room server. Members discover one another with mDNS and send chat messages directly over TCP. There is no leader, standalone daemon, account, database, TUI, or Web UI.

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
| macOS / arm64 | `bin/darwin_arm/lanlobby` |
| Linux / amd64 | `bin/linux/lanlobby` |
| Windows / amd64 | `bin/windows/lanlobby.exe` |

For example, run `./bin/darwin_arm/lanlobby --help` on Apple Silicon. On Windows, run `.\\lanlobby.exe --help` after copying the Windows artifact. `make clean` removes `bin/`.

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

## Usage

### Join and listen

Terminal A:

```bash
lanlobby join room1 --as agent-a
```

This creates or joins `room1` and keeps listening in the foreground. Status goes to stderr; stdout contains only live messages:

```text
agent-a: hello
agent-b: hi
```

Start another member in a different terminal, on this machine or another machine on the same LAN:

```bash
lanlobby join room1 --as agent-b
```

A room has no owner. It exists while members are online and disappears from discovery when its last member leaves.

### Send from another terminal

While `agent-a`'s join is still running, use another terminal **on the same machine and under the same OS user**:

```bash
lanlobby send room1 --as agent-a "hello"
```

Successful sends print nothing. Messages appear in the joined terminals, including the sender's own terminal once.

`send` talks to the matching local join through an owner-only Unix socket on macOS/Linux, or a current-user-only named pipe on Windows. It does **not** join implicitly, start a daemon, or let a separate machine send just by claiming `--as agent-a`.

Press **Ctrl-C** in the join terminal to leave. That membership can no longer send or receive; later `send` commands for it fail until it joins again. Messages already accepted or in flight cannot be recalled.

For a message beginning with `-`, use `--`:

```bash
lanlobby send room1 --as agent-a -- "--still a message"
```

### Discover rooms and members

```bash
lanlobby list room
# ROOM   USERS
# room1  2

lanlobby list user --room=room1
# USER     ADDRESS
# agent-a  127.0.0.1:53101
# agent-b  192.168.1.20:53102
```

These are finite, sorted snapshots, not watches, and they do not join a room. Empty results print the header and exit successfully. Every listed member has answered a TCP probe for its exact current session; old mDNS records alone do not count as online. Local members prefer loopback, so their address may be `127.0.0.1`.

A snapshot normally takes about **1.5 seconds**, plus TCP verification. Sending takes a fresh snapshot too. mDNS discovery is best effort: a snapshot may miss a member, especially during startup or packet loss.

### Select a network interface

By default, discovery uses active, multicast-capable, non-loopback IPv4 interfaces. On machines with VPNs or multiple NICs, select the LAN interface explicitly:

```bash
# macOS example; on Linux this might be eth0 or wlan0.
lanlobby join room1 --as agent-a --interface en0
lanlobby list room --interface en0
lanlobby list user --room=room1 --interface en0
```

`send` uses the network configuration of its existing join, so it has no `--interface` flag. The interface option selects mDNS interfaces; it is not a TCP access-control boundary.

## Output and delivery

- No prompts, ANSI colors, cursor movement, or full-screen interface. `join` does not read stdin; send through a separate invocation.
- stdout is a line-oriented `NAME: MESSAGE` stream. Embedded newlines, terminal controls, and Unicode formatting characters are visibly escaped so a message occupies one output line.
- Status, usage errors, and delivery errors go to stderr.
- Exit codes: `0` success/help/clean interruption, `1` runtime or delivery error, `2` invalid command or arguments.
- Names are 1–64 UTF-8 bytes without whitespace, control, or formatting characters. Messages are 1–8 KiB of UTF-8 text.
- Only freshly discovered, TCP-verified members of the same room are recipients. Recipients do not relay messages. There is no global message ordering.
- Each recipient is contacted once. Messages are never automatically retried; a lost acknowledgment may leave delivery uncertain. A failed or partial send returns nonzero and explains the failure on stderr.
- Delivery is not atomic across the room. A sender's own echo and some recipients' output can appear even when another recipient fails. A successful send with no discovered recipients only echoes locally.
- An acknowledgment means acceptance into a bounded live output queue, **not** durable storage or proof that a person has read the message. Slow consumers can fill the queue and cause a send to fail.

## No history

There is no persistence, replay, catch-up, offline queue, or automatic resend. A late joiner sees only new messages. Leaving and rejoining creates a fresh session and never restores old output.

While joined, there is a bounded in-memory queue for immediate display. The CLI itself does not write chat text to disk. Redirecting stdout or capturing terminal output is, of course, up to the caller.

On macOS/Linux, local coordination uses `/tmp/lanlobby-<uid>` with mode `0700`. Socket files are removed on normal exit; a later join can safely reclaim its stale socket after SIGKILL. Empty `.lock` files intentionally remain to preserve cross-process locking. These contain no messages, names, or history; filenames are hashes of the room/user pair.

On Windows, an exclusive named-pipe listener holds membership. Pipe names incorporate the Windows user SID and a hash of the room/user namespace. The protected ACL grants only that user access, clients verify the pipe owner before sending, and remote pipe clients are rejected. Closing the process releases its pipe handles; no runtime socket or lock files are created.

## Network and trust boundary

- **Discovery:** mDNS/DNS-SD service `_lanlobby._tcp.local.`, normally UDP 5353 on `224.0.0.251`.
- **Chat and liveness:** direct IPv4 TCP to an OS-assigned port for each foreground join. Chat content never travels in UDP advertisements.
- No internet service is used at runtime. Both multicast discovery and inbound TCP must be permitted by the network and host firewall. Guest Wi-Fi isolation, VPN routing, VLAN boundaries, and macOS local-network permissions can prevent discovery or TCP connections.
- This version requires a working IPv4 multicast LAN; it has no static-peer fallback or IPv6-only mode.
- **Trusted LAN only: no encryption, passwords, cryptographic identities, or authenticated room membership.** Room names, usernames, addresses, and session IDs are visible in discovery. Chat travels in plaintext. Anyone with network access can potentially read or forge traffic; room names are not secrets.
- Local duplicate names in the same room are rejected under the same OS account. LAN-wide same-name conflicts are detected best effort: the lexicographically smaller random session ID survives. This is not global consensus and cannot guarantee uniqueness during a network partition. Online checks are lifecycle checks, not authentication.

If `list` is empty, check both multicast and TCP reachability: announcements can be visible even when the advertised TCP address is blocked. `--interface` can help avoid an unsuitable VPN interface. Reported listener/setup errors return nonzero, but silent network filtering can still look like an empty LAN.

## Development and verification

```bash
go fmt ./...
go vet ./...
go test ./...
go test -race ./...
```

Ordinary tests use deterministic discovery with real loopback TCP and platform-local IPC, including process-termination recovery. Unix filesystem/flock tests and Windows named-pipe tests are platform-specific. They do not require LAN multicast.

Opt into real mDNS and multi-process CLI checks on a connected LAN:

```bash
LANLOBBY_LAN_TEST=1 go test -count=1 -v ./integration ./internal/discovery
```

The CLI test builds and launches the actual executable, checks discovery and bidirectional messages, room isolation, duplicate joins, send rejection after leaving, and no history on rejoin. It checks Ctrl-C/SIGKILL on Unix; on Windows it uses explicitly labeled forced termination, since Go cannot send `os.Interrupt` to another Windows process. Real Windows console Ctrl-C needs manual verification. All test processes are stopped afterward. Same-host TCP may use loopback; **this does not replace a two-machine LAN test**.

`make build` cross-builds all three artifacts with CGO disabled. A successful cross-build is not runtime verification: Windows/Linux execution, Windows console Ctrl-C and firewall/multicast behavior, and two-machine LAN communication must be verified on the respective machines.

### Two-machine smoke test

1. Build/copy the appropriate binary to two machines on the same multicast-capable LAN.
2. On machine A, run `lanlobby join room1 --as agent-a` and keep it open.
3. On machine B, run `lanlobby join room1 --as agent-b` and keep it open.
4. From another terminal, run both list commands and confirm both members appear. If needed, use each machine's own `--interface` value for join/list.
5. From a second terminal on A, send as `agent-a`. Confirm one line on both machines. Send a reply as `agent-b` from a second terminal on B.
6. Join a late member and confirm no earlier output appears. Join another room and confirm it receives none of `room1`'s messages.
7. Press Ctrl-C on B's join. Sending as `agent-b` on B must fail, and a new list snapshot must omit it.
8. Rejoin B. Confirm old messages are absent and new messages work. Finally, leave all joins and confirm `room1` is absent from a new room snapshot.
