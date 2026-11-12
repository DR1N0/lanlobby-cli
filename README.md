# lanlobby-cli

Live LAN chat for agents and terminals. **Pure CLI. Direct TCP. No central server. No history.**

Each `join` is a foreground member, not a connection to a room server. Members discover one another with IPv4 UDP multicast queries and unicast replies, then send chat messages directly over TCP. There is no leader, standalone daemon, account, database, TUI, or Web UI.

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
lanlobby version --short
# v0.0.1
```

`make build` runs `hack/generate-ldflags.sh` and injects the version tag, Git commit, worktree state, and UTC build date with Go `-ldflags -X`. The full command also prints the Go version and target platform. It is local-only: no LAN discovery or update-server requests.

The initial `BUILD_TAG` is `v0.0.1`. Override it for later builds with `make build BUILD_TAG=v0.0.2` or `BUILD_TAG=v0.0.2 make build`. All three artifacts in one build share the same metadata. This embeds a version; it does not create Git tags or publish a release.

A plain `go build`/`go install` uses the source default version and reports uninjected commit/state/build-date fields as `unknown`.

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

These are finite, sorted snapshots, not watches, and they do not join a room. Empty results print the header and exit successfully. Every listed member has answered a TCP probe for its exact current session; a UDP advertisement alone does not count as online. Local members prefer loopback, so their address may be `127.0.0.1`. Local reachability does **not** prove that another machine can reach them.

A snapshot normally takes about **1.5 seconds**, plus TCP verification. Queries repeat at approximately 0, 300, and 900 ms, and the browser keeps collecting even after discovering itself. Each snapshot uses its own query ID and temporary UDP ports; simultaneous list/send/conflict checks do not share reply sockets. There is no cross-snapshot cache. Sending takes a fresh snapshot too. Discovery is best effort: a snapshot may miss a member, especially during startup or packet loss.

**Discovery compatibility:** this protocol replaces the earlier mDNS implementation. Old and new builds cannot discover each other. Update every participating machine, even if both binaries report `v0.0.1`.

### Select a network interface

By default, discovery uses active, multicast-capable, non-loopback IPv4 interfaces. On machines with VPNs or multiple NICs, select the LAN interface explicitly:

```bash
# macOS example; on Linux this might be eth0 or wlan0.
lanlobby join room1 --as agent-a --interface en0
lanlobby list room --interface en0
lanlobby list user --room=room1 --interface en0
```

On Windows, find the actual interface name with PowerShell `Get-NetAdapter`, then use, for example, `--interface "Wi-Fi"` or `--interface "Ethernet"`; `en0` is a macOS example, not a portable name.

`send` uses the network and diagnostic configuration of its existing join, so it has no `--interface` or `--debug` flag. The interface option selects discovery interfaces and explicitly configures multicast egress; it is not inbound filtering or a TCP access-control boundary. Wildcard multicast receivers can see duplicate packets across interfaces; candidates are deduplicated by session and their source addresses are merged before probing. OS routing/VPN policy still applies to unicast replies and TCP.

### Diagnose discovery

```bash
lanlobby join room1 --as agent-a --interface en0 --debug
lanlobby list room --interface en0 --debug
lanlobby list user --room=room1 --interface en0 --debug
```

`--debug` writes selected interfaces/IPs, sent queries, received candidates, TCP verification results, and snapshot counts to **stderr only**. stdout remains a plain message stream or list. It never logs chat bodies. Diagnostics use a bounded, best-effort queue: a slow/blocked stderr can drop diagnostic/status lines, but cannot hold network sockets open on leave.

- `packets=0` / `candidates=0`: no usable reply was observed; check the multicast query path, member responder, and unicast UDP return path.
- `rejected>0`: packets arrived but failed packet/version/query-ID/source checks.
- Candidates followed by `TCP verification failed`: UDP discovery worked for those candidates, but their advertised session did not pass TCP verification. Such members are intentionally omitted.
- Warnings name failed interfaces/socket operations. If no interface can operate, the command fails instead of reporting an ordinary empty LAN. If all active discovery responders fail, join exits and releases its local membership.

A successful UDP send only means the OS accepted the datagram; it does not prove delivery. Silence alone does not identify a firewall, VPN, permission, or implementation fault.

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

- **Discovery queries:** project-specific IPv4 UDP multicast to **`239.255.42.99:53318`**, TTL 1, multicast loopback enabled, explicit outgoing interface. Each foreground join listens on the shared reusable discovery port; no separate discovery service runs.
- **Discovery replies:** each join unicasts JSON metadata to the query's observed source IP and temporary UDP port. Replies carry a matching random query ID, session ID, room, user, and TCP port. Peer addresses come from the UDP packet source, not a payload-provided address. Packets are capped at 1200 bytes; candidate counts, address counts, and reply rates are bounded. Query IDs and source addresses are **not authentication**.
- **Chat and liveness:** direct IPv4 TCP to an OS-assigned port for each foreground join. Chat content never travels in UDP advertisements.
- No internet service is used at runtime. Network and host firewall rules must permit **inbound multicast UDP 53318**, **unicast replies to temporary UDP query ports**, and **inbound TCP to each join's temporary port**. Do not assume a firewall treats unicast replies as established traffic merely because a multicast query was sent. On a trusted Windows Private LAN, prefer a program-specific inbound allowance over disabling the firewall globally.
- Guest Wi-Fi/AP isolation, VPN routing, VLAN boundaries, and macOS local-network permissions can prevent reception or TCP connections. No system firewall, routing, VPN, or privacy settings are changed by the CLI. An absent permission-list entry alone does not establish whether traffic is denied.
- This version requires a working IPv4 multicast LAN; it has no static-peer fallback or IPv6-only mode.
- **Trusted LAN only: no encryption, passwords, cryptographic identities, or authenticated room membership.** Room names, usernames, addresses, and session IDs are visible in discovery. Chat travels in plaintext. Anyone with network access can potentially read or forge traffic; room names are not secrets.
- Local duplicate names in the same room are rejected under the same OS account. LAN-wide same-name conflicts are detected best effort: the lexicographically smaller random session ID survives. This is not global consensus and cannot guarantee uniqueness during a network partition. Online checks are lifecycle checks, not authentication.

If `list` is empty, use `--debug` to separate query/reply discovery from TCP verification. Advertisements can be visible even when the advertised TCP address is blocked. `--interface` can help avoid an unsuitable VPN interface, but is not a bypass for OS policy. Reported listener/setup errors return nonzero; silent filtering can still look like an empty LAN. There is no old-mDNS fallback, subnet scan, or HTTP registration.

## Development and verification

```bash
go fmt ./...
go vet ./...
go test ./...
go test -race ./...
```

Ordinary tests use deterministic discovery, loopback UDP/TCP, and platform-local IPC, including process-termination recovery. They cover protocol/nonce validation, retries, concurrent browsers, bounded candidates/replies/diagnostics, socket failure and cancellation, and discovery failure releasing membership. Unix filesystem/flock tests and Windows named-pipe tests are platform-specific. They do not require LAN multicast.

Opt into real multicast fanout, unicast reply, and multi-process CLI checks on a connected LAN:

```bash
LANLOBBY_LAN_TEST=1 go test -count=1 -v ./integration ./internal/discovery
```

The CLI test builds and launches the actual executable, checks discovery and bidirectional messages, room isolation, duplicate joins, send rejection after leaving, and no history on rejoin. It checks Ctrl-C/SIGKILL on Unix; on Windows it uses explicitly labeled forced termination, since Go cannot send `os.Interrupt` to another Windows process. Real Windows console Ctrl-C needs manual verification. All test processes are stopped afterward. Same-host TCP may use loopback; **this does not replace a two-machine LAN test**.

`make build` cross-builds all three artifacts with CGO disabled. A successful cross-build is not runtime verification: Windows/Linux execution, Windows console Ctrl-C and firewall/multicast behavior, and two-machine LAN communication must be verified on the respective machines.

### Three-machine smoke test (two Macs and Windows)

1. Run `make build` and copy the **new** platform artifact to all three machines on the same multicast-capable LAN. Use an appropriate architecture (the initial Mac artifact is arm64). Verify `--help` includes `--debug`; the version string alone cannot distinguish the old mDNS build.
2. Keep one join open on each machine: Mac A `lanlobby join mac-a --as agent-a --debug`; Mac B `lanlobby join mac-b --as agent-b --debug`; Windows `.\\lanlobby.exe join win-pc --as agent-c --debug`.
3. On **each machine**, run `lanlobby list room --debug` (Windows: `.\\lanlobby.exe list room --debug`). Every snapshot should show all three rooms, once each. If needed, set each machine's actual LAN `--interface` on both join and list. Keep stderr separate from stdout when collecting diagnostic evidence.
4. From extra terminals on each machine, also join a shared room `room1`, with distinct names `shared-a`, `shared-b`, and `shared-c`. `list user --room=room1 --debug` should show all three on each machine. Run multiple list commands simultaneously to check independent replies.
5. Send from another terminal on the **same machine/user** as each matching join: `lanlobby send room1 --as shared-a "hello from A"`, and likewise for B/C. Confirm exactly one new line on all three shared-room terminals and no output in the other rooms.
6. Add a late join and confirm no earlier output appears. Press Ctrl-C on B's shared join: sending as `shared-b` on B must fail, and fresh snapshots should omit it. Rejoin B and verify no old messages, only new ones. Test actual Windows console Ctrl-C too; cross-compilation cannot verify it.
7. Finally, leave every join; a fresh snapshot should show none of the test rooms. Success on one host or ordinary tests is not a substitute for these cross-machine results.
