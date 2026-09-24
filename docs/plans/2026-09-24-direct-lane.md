# Direct lane for push and pull, 2026-09-24

Status: implemented on this branch; real-link acceptance in progress. Revised
after two design reviews, one code review and two real-link runs (see "Review
response" at the end).

## Why

Every byte of a push or pull crosses the relay: sender, CDN edge, tunnel, relay,
tunnel, CDN edge, receiver. After v0.13.0 fixed the receiving side, both
directions are bounded by the same thing, the sender's upload into the CDN.
Measured on 2026-09-24 with v0.13.0, all through the CDN:

| path | result |
|---|---|
| controller on an office network, device on a mainland home line, 30 MB push / pull | 2.4 / 2.6 MB/s |
| the home line uploading into the CDN, raw POST, any request shape | 0.6–5.4 MB/s, minute to minute |
| the same home line to a domestic server, no CDN | 17 MB/s |
| office controller to home device, UDP hole punched, QUIC over the hole | punched in 1.2 s, QUIC handshake 80 ms, 4.7–4.9 MB/s both ways (the office network's own cap) |

Request shape, tunnel replica choice, cloudflared's 1 MiB HTTP/2 window and
parallel connections were each ruled out as the limit. The only lever left is
not sending the bytes through the CDN at all. A controller and a device on the
same LAN are the extreme case: today they move data at CDN-upload speed.

## Scope of the first version

In: the data phase of `push` and `pull` of files of at least `directMinBytes`
(8 MiB), between a controller and a device that are both this version or later,
in an ordinary paired session.

Out, each deliberately:

- exec, logs, file_read/edit/write and anything small. Their cost is session
  setup round trips, which a direct path does not remove.
- delegated (`GrantID` set), workspace and console sessions.
- TURN or any relay of our own on the direct path. The relay path is the
  fallback.
- symmetric-NAT port prediction; UPnP / NAT-PMP / PCP.
- switching path in the middle of a transfer, and retrying a failed transfer
  automatically.
- a user-facing option. `WANCTL_DIRECT=0` on either side disables the lane, for
  incident response only.

## Wire format

One new optional field on `protocol.Message`, one struct used in both
directions, two new message kinds on the relay session and one on the direct
stream.

```go
// DirectInfo is one side's part of a direct-lane negotiation.
type DirectInfo struct {
	// "ip:port" or "[ipv6]:port". At most 8 entries, each at most 64 bytes.
	Candidates []string `json:"candidates,omitempty"`
	// base64url without padding of SHA-256 over the leaf certificate's DER.
	CertSHA256 string `json:"cert_sha256,omitempty"`
}

Message.Direct *DirectInfo `json:"direct,omitempty"`

KindDirectOffer    = "direct_offer"    // controller -> device, relay session
KindDirectFallback = "direct_fallback" // controller -> device, relay session
KindDirectAttach   = "direct_attach"   // controller -> device, first frame on the direct stream
```

Meaning by position:

| message | `direct` |
|---|---|
| `file_put` / `file_get` from the controller | `{}`: the controller can use the lane for this operation. Absent: it cannot. |
| `ok` (the `file_put` ack) / `file_meta` from the device | `{candidates, cert_sha256}`: the device offers the lane and is now holding the operation. Absent: proceed on the relay as today. |
| `direct_offer` | `{candidates, cert_sha256}` of the controller. |
| `direct_fallback`, `direct_attach` | absent. |

Old agents decode messages with plain `json.Unmarshal`
(`internal/protocol/protocol.go:302`, `:309`), which ignores the unknown field,
so an old agent never answers. A controller sends `direct_offer`,
`direct_fallback` and `direct_attach` only after it has received the device's
`DirectInfo`, so no old agent ever receives an unknown kind. A new agent that
receives `direct_offer` or `direct_fallback` outside a hold treats it like any
unknown request today (error, session closed).

## Negotiation, for push and pull alike

The relay session is set up exactly as today (dial, E2E mutual TLS, hello,
TOFU). Nothing below runs until the device has gated the operation.

1. **Controller** sends the operation with `direct: {}`: for `file_put` when the
   size is at least `directMinBytes` (8 MiB, so always above the 64 KiB
   pipelined-upload path); for `file_get` always, because only the device knows
   the size. No socket is opened and no STUN is sent at this point, so small
   pulls cost nothing.
2. **Device** gates the operation exactly as today: policy, approval, audit, once.
   It answers with `DirectInfo` only if all hold:
   - `directAllowed` for this session, computed in `handleSession` from
     authenticated state: `auth.GrantID == ""`, the hello was `KindHello` (not
     console, not workspace), and the lane is enabled. It is passed into
     `serveAuthorized` and never inferred from message fields;
   - the controller sent `direct: {}`;
   - the file is at least `directMinBytes` (push: declared size; pull: size of the
     opened file);
   - fewer than `directMaxActive` (4) operations are holding or transferring
     directly on this agent;
   - after opening its sockets and gathering (at most 500 ms, see below) it has
     at least one candidate. With none it closes the sockets and answers as
     today, without `direct`.

   It then generates a certificate and sends the ack / `file_meta` with
   `DirectInfo`. From here the operation is **holding**: for push the pending
   upload is open with nothing written; for pull the policy-bound file is open
   and nothing has been sent.
3. **Controller**, on receiving `DirectInfo`: opens its sockets, gathers
   candidates (at most 500 ms), generates a certificate, sends `direct_offer`
   on the relay, and starts punching and dialling. The direct budget is 2.5 s
   from sending `direct_offer`. If the device's list is empty after
   validation, or it cannot gather anything usable itself, it sends
   `direct_fallback` instead of `direct_offer`.
4. **Device**, on `direct_offer`: validates the controller's candidates, starts
   its QUIC listener and punches towards them (at most 5 s of probing). During
   the hold it may accept up to 8 authenticated connections from concurrent
   candidate dials. The first connection carrying `direct_attach` is selected;
   the others are closed.
5. **Controller** decides, once. Only the controller selects the path, and it
   sends exactly one of two signals per operation:
   - A QUIC connection completed within the budget: it opens the one stream,
     writes `direct_attach`, and from then on never sends `direct_fallback`.
   - Otherwise: it cancels every dial, closes its transports, then sends
     `direct_fallback` on the relay and flushes the carrier. On the HTTP
     carrier, writes within 5 ms share one upload that the relay forwards only
     when complete; without the flush a push's first megabyte of file data
     travelled with the fallback and a slow uplink outlasted the device's hold.
6. **Device** takes whichever signal arrives, through one arbiter (a single
   `select` fed by the relay reader and the direct acceptor):
   - `direct_attach` on an authenticated stream: selection is final. It writes
     `ok` on the stream, closes its listener to further connections, and runs
     the data phase on the stream. A `direct_fallback` that arrives on the relay
     after this is answered with an error and the relay session is closed; the
     direct transfer is unaffected.
   - `direct_fallback` on the relay: selection is final. It closes the listener
     and any accepted connection, and runs the data phase on the relay exactly
     as today.
   - Neither within `directHold` (30 s from sending `DirectInfo`): it writes an
     error on the relay, aborts the operation and closes everything.
7. **Controller**, after `direct_attach`, waits up to 5 s for the device's `ok`
   on the stream. If it does not come, the operation fails; the controller
   does not fall back, because the device may already have selected direct.

The data phase is the existing framing, unchanged, on whichever path was
selected:

- push: controller writes `FrameData ... eof`; device commits and writes the
  final `ok` (with `size`) or `error` on the same path.
- pull: device writes `FrameData ... eof`, or `error` if reading the source
  fails (today it returns silently; that changes on both paths).

### State rules

These close the questions the second review left open.

**Who reads the relay connection.** Exactly one reader at any time, using the
pattern `doExec` / `watchPeer` already use (`internal/agent/agent.go:707`,
`:733`): a one-shot goroutine performs a single `protocol.ReadMessage` and
delivers it on a channel; whoever holds the channel owns the read.

- While holding, the file handler owns the relay read. It waits for
  `direct_offer` or `direct_fallback` through one such one-shot read at a time;
  anything else on the relay is a protocol error (error written, operation
  aborted, session closed).
- `direct_fallback` arrives: that read has completed, no read is outstanding,
  and the handler reads the data phase synchronously, exactly as today.
- `direct_attach` selects direct, or the hold times out: a relay read is still
  outstanding. The handler returns its channel to `serveAuthorized` as that
  loop's pending read, the way `doExec` does. It never starts a second read.

**Who sends the selection.** On the controller, the operation's goroutine alone
writes `direct_attach` or `direct_fallback`. Dial workers only report
connections on a channel. When the budget ends it cancels their context, waits
for all of them to return (closing any connection they report late), closes
its transports, then writes `direct_fallback`.

**When the device commits to direct.** On `direct_attach` over an authenticated
stream it first registers the worker with the agent's lifecycle (`enter`, the
same gate `spawn` uses; if shutdown has begun it writes `error` on the stream
and closes). Only then does it write `ok` on the stream. The worker holds one of
the `directMaxActive` slots until the data phase ends and its result is written.
The file and slot are released then; the QUIC connection can close separately.

**What the relay session is for after selection.** Nothing in the transfer
depends on it. On the device the request loop goes on reading it (through the
handed-back read), so its HTTP polls keep it alive; on the controller the
operation does not read it and closes it when the operation ends, as today.
A relay failure or the controller closing the relay session does not stop a
selected direct transfer; the controller cancelling does (below).

**Revocation.** A token or pairing revoked while a selected direct transfer runs
takes effect for the next operation, not the running one. This is the
semantics ordinary sessions already have on the WebSocket carrier, where the
relay re-checks credentials mid-session only for delegated access
(`internal/relay/delegation_auth.go:143`); delegated sessions never use the
lane.

**Limits on a running transfer.** At most `directMaxActive` (4) operations
holding or transferring per agent. A selected transfer is aborted after 30 s
without file data being written to the pending upload (push) or read from the
file and sent (pull). Frame headers, control messages and QUIC keepalives do
not count as progress. There is no total-duration cap.

### After selection

- The direct data phase runs in its own goroutine under the agent's run
  context and the operation context below.
- One operation context on each side governs dials, listener, connection,
  stream, file and pending upload. Controller cancellation (Ctrl-C, MCP call
  cancelled) closes the QUIC connection with an application error; the device
  sees the stream reset and aborts the pending upload or closes the file.
  Agent shutdown cancels the run context and does the same. `Pull` gains the
  `CloseOnCancel` that `Push` already has (`internal/client/files.go:59`).
- A dead peer is detected by QUIC's idle timeout (15 s with 5 s keepalives).
  There is no total-duration cap: a 1 GiB transfer on a slow path is legitimate.

### Failures are reported, not retried

A direct data phase that breaks fails the operation. Push commits by rename
before sending its final `ok` (`internal/server/files.go:76`, `:171`), and
`pushReader` takes a non-rewindable `io.Reader`, so a retry could repeat a
committed write and ask the owner to approve twice. The error says which path
carried it and, for push, whether the outcome is unknown: a break after the
controller wrote `eof` and before the result arrived means the device may have
committed. The user re-runs.

### Pull checks the size

Pull keeps today's way of writing the destination on both paths (open with
truncate, write in place), so symlinks, hard links and permissions behave as
they do now. What changes: at `eof` the controller requires the received count
to equal `file_meta.Size`, and any failure after the destination was opened
says the local file is incomplete and names it. Integrity in transit is QUIC's
(direct) or TLS's (relay); no digest is added to the protocol.

## The direct path

### Sockets

Per negotiation each side opens up to two UDP sockets, each wrapped in its own
`quic.Transport`: one `udp4` bound to `0.0.0.0:0`, and one `udp6` bound to
`[::]:0` if the host has a global or unique-local IPv6 address. No dual-stack
socket: quic-go's own tests note flaky dial-ups on macOS dual-stack sockets.
A candidate is dialled or probed through the transport of its family.

Before the first STUN request on a transport, the owner calls
`ReadNonQUICPacket` once with an already-cancelled context. That call sets
quic-go's "reading non-QUIC packets" flag synchronously before returning
(`transport.go:714`); packets arriving before the flag is set are dropped.
Only then does the reader goroutine start and the first request go out.

Teardown closes each `quic.Transport` and then its `net.UDPConn` explicitly
(the transport does not close a caller-owned socket). If quic-go reports that
it could not enlarge the UDP buffers, that fact is kept for diagnostics.

### Candidates

- **host**: every up, non-loopback interface's unicast addresses that pass the
  validation below. On Android 11+, where `net.Interfaces` is refused, the
  default-route probe below stands in.
- **server-reflexive**: STUN Binding (RFC 5389, hand-rolled, no dependency)
  sent to all servers at once on each transport; answers are accepted only if
  the transaction ID matches one we sent and the source address is the server
  we sent it to; first two distinct answers within 500 ms. Mappings that
  differ by server (a multi-homed office sends some destinations out of
  another line) are all kept. Default servers, from what answered in the
  mainland on 2026-09-24 and what Syncthing / EasyTier / Natter ship:
  `stun.miwifi.com:3478`, `stun.chat.bilibili.com:3478`, `stun.hitv.com:3478`,
  `stun.l.google.com:19302`, `global.stun.twilio.com:3478`,
  `stun.cloudflare.com:3478`. (`stun.qq.com` did not answer from three
  networks.)
- Order and cap: own candidates are deduplicated and capped at 8, in this order:
  the IPv4 and IPv6 source addresses the OS would use for the default route
  (the local address of a UDP socket connected, without sending, to
  `1.1.1.1:53` / `[2606:4700:4700::1111]:53`); server-reflexive IPv4 (up to 2)
  and IPv6 (up to 1); then remaining host addresses, private ranges first,
  skipping point-to-point interfaces. This keeps the LAN address a same-LAN
  peer needs ahead of container bridges and VPNs.
- Overlay addresses are never candidates: `100.64.0.0/10` (Tailscale, and
  carrier-grade NAT space a peer cannot reach) and Tailscale's
  `fd7a:115c:a1e0::/48`. On Windows the Tailscale adapter is not flagged
  point-to-point, and when the tailnet itself is relaying through DERP a
  handshake over it completes but the transfer runs at tens of kilobytes per
  second.
- A `udp6` socket is opened only if a global or unique-local IPv6 address was
  found by either method. On Android 11+ that means the default-route probe
  only; an Android device with unique-local IPv6 but no IPv6 default route
  uses IPv4 in this version.

Validation, applied to the peer's list before any packet is sent to it:
at most 8 entries, each at most 64 bytes, each parses as `ip:port` with port
1–65535; the IP is unicast and not loopback, unspecified, multicast,
limited broadcast, or link-local; IPv4-mapped IPv6 is normalised to IPv4. A
list that fails validation is treated as empty (the controller then sends
`direct_fallback`; the device answers the offer by waiting for fallback).
Overlay addresses in a peer's list are well formed and are dropped one by one
instead, so a peer that advertises them keeps its other paths.

### Punching and dialling

The controller dials, the device listens. Both send 64-byte random probes whose
first byte has its two high bits clear (so quic-go routes them to the non-QUIC
queue) to every peer candidate, on go-libp2p's schedule: sleep
10 ms + rand(min(10(i+1)², 200)) ms between rounds. The controller stops at
selection or its 2.5 s budget; the device stops at selection or 5 s after
`direct_offer`. With 8 candidates that is at most a few hundred probes, some
tens of kilobytes, only to addresses the authenticated peer supplied.

The controller also calls `Transport.Dial` to every peer candidate at once.
While those dials run, it consumes the non-QUIC packet queue. A packet with
the STUN magic cookie (`0x2112A442` at bytes 4–8) is not a probe. On the first
64-byte probe from an address, the controller starts an additional QUIC dial
to that exact address, even if an initial dial to the candidate is already in
flight. A peer-reflexive source not in the candidate list is eligible only if
it passes the same unicast-address validation; at most 8 additional addresses
are dialled. This avoids waiting for an Initial PTO after the device's NAT
mapping opens. The first handshake to complete wins; all other dials are
cancelled and their connections closed.

QUIC configuration, both sides: `KeepAlivePeriod` 5 s (Windows removes an idle
UDP flow after 60 s), `MaxIdleTimeout` 15 s, `MaxIncomingStreams` 1,
`MaxIncomingUniStreams` -1 (none), no 0-RTT (`Listen` and `Dial`, not the Early
variants; `Allow0RTT` false), ALPN `wanctl-direct/1`, default flow-control
windows.

### Authentication

Each side generates a fresh Ed25519 self-signed certificate per negotiation and
sends the hash of its leaf DER inside the E2E session. Verification happens in
the TLS handshake, not after it:

- device listener: `ClientAuth: tls.RequireAnyClientCert` and a
  `VerifyConnection` that fails unless the peer's leaf DER hashes to the
  controller's pin and the negotiated ALPN is `wanctl-direct/1`;
- controller dialler: `InsecureSkipVerify: true` (there is no CA) and a
  `VerifyConnection` that fails unless the server's leaf DER hashes to the
  device's pin and the ALPN matches.

The existing `internal/transport` helpers are not reused here: they accept any
client certificate and authorise its fingerprint after the handshake
(`internal/transport/conn.go:11`, `handshake.go:51`), which on QUIC would let
an arbitrary certificate open a connection and a stream.

Because the pins were exchanged inside the already-authenticated E2E mutual
TLS session (the relay admitted it, TOFU accepted the controller, policy
accepted the operation), a connection that passes both checks is bound to
this controller and this operation, and carries the data phase without an
inner TLS layer. This is the arrangement WebRTC uses with DTLS fingerprints in
signalling. The reviewer's verdict on it is recorded below.

### Exposure of the listener

The listener exists from `direct_offer` until selection or `directHold`. It
accepts at most 8 authenticated connections during the hold because concurrent
candidate dials can complete in a different order on the two sides. The first
one carrying `direct_attach` wins the one data stream; the other connections
are closed and the listener is closed to further connections. Before the
certificate check, quic-go processes Initial packets
from anyone who can reach the socket, and the source address it sees is not
yet validated. The transport's `VerifySourceAddress` returns false (no Retry)
when the source IP, after normalising IPv4-mapped IPv6, equals the IP of one of
the controller's validated candidates, and true (Retry) otherwise. This is a
cost optimisation, not a proof of origin: it saves the expected peer a round
trip and makes everyone else pay one. A peer-reflexive controller address pays
one extra round trip inside the 2.5 s budget. An Internet sender can make the
device process Initials and send Retries for at most 30 s per operation; it
cannot reach application data.

## Compatibility

| controller | device | result |
|---|---|---|
| old | any | no `direct: {}`, relay, as today |
| new | old | field ignored, no answer, relay, as today |
| new | new, lane disabled or session not eligible | no answer, relay |
| new | new | direct if a QUIC connection completes within 2.5 s of `direct_offer`, else relay |

The relay is unchanged.

## Costs and risks

- A large transfer that cannot go direct starts its data up to about 3.5 s
  later than today (device gathering ≤ 0.5 s, controller gathering ≤ 0.5 s,
  budget 2.5 s). Below 8 MiB nothing changes, and small pulls open no socket.
- STUN servers are third parties and learn the public address of every device
  and controller that negotiates. Self-hosting needs a machine with a public
  address that passes UDP; the relay sits behind a CDN and cannot.
- Windows: the Defender Firewall prompt is documented against listening
  sockets; whether binding a UDP socket from an agent in a desktop session
  raises it is not documented, nor whether the block rule a dismissed prompt
  creates stops outbound-first flows. Measured only from a session-0 process
  so far: no prompt, no rule, inbound QUIC after punching worked.
- Candidates reveal a device's LAN and public addresses to a controller it has
  paired, and the controller's to the device.
- Success depends on NAT type. Tailscale reports over 90% direct with basic
  techniques; libp2p's large measurement (arXiv 2510.27500) found 70% given
  address discovery succeeded. Two symmetric NATs will not punch.

## Observability

The CLI's final line adds `, direct` only when direct carried the data;
the relay line is unchanged. A missing `DirectInfo` prints nothing, preserving
old-peer and disabled-lane output. Once a direct attempt has started, a
fallback prints one stderr line saying why (no valid candidates, UDP socket
unavailable, or punching timed out); a failure says which path broke and,
for push, whether the outcome is unknown. The device logs one event per negotiation:
`direct` with `established:<candidate type>` (host / ipv6 / reflexive) or
`fallback:<reason>`.

## Acceptance

Written before implementation; measured on real devices, not tests. "Hash" means
the tester compares SHA-256 of source and destination.

- Home LAN, Mac and the Windows box: 100 MB push and pull at least 10× faster
  than v0.13.0 on the same pair, hash equal.
- Office Mac to home Windows box: 30 MB push and pull at least 4 MB/s (v0.13.0:
  2.4–2.6), hash equal.
- UDP blocked on either side: falls back; total time within relay time + 4 s
  (3.5 s of budgets plus margin for sockets, DNS and scheduling); hash equal.
- 1-byte push and 64 KiB pull latency unchanged versus v0.13.0 (interleaved,
  medians).
- Old agent with new controller and new agent with old controller: unchanged
  behaviour.
- Ctrl-C during a direct push leaves no file and no pending upload on the
  device; during a direct pull the CLI says the local file is incomplete.
- A controller that attaches and then stops sending: the device aborts the
  operation 30 s after the last file byte.
- Windows agent in a desktop session, standard user: record whether a firewall
  prompt appears, and whether direct still works after dismissing it.
- Record which pairs among the available devices (Android phone, a cloud VM,
  the Linux VM) punch.

## Evidence

A standalone Go probe (quic-go v0.63.0, one UDP socket shared between STUN and
QUIC through `ReadNonQUICPacket` / `WriteTo`) asked six STUN servers for the
socket's mapping from three networks, then punched between an office Mac and a
home Windows machine using each side's domestic-STUN mapping and ran 32 MiB
each way over QUIC on the punched socket. Upload ceilings through the CDN were
measured with a throwaway sink behind the same tunnel, uploading 32 MiB with
the agent's HTTP/3 settings under several request shapes, interleaved.

Precedents for the socket arrangement: Syncthing (`quic_listen.go`, STUN on the
QUIC transport) and go-libp2p (`p2p/transport/quic/transport.go`, `holePunch`).

The first branch build was tried between an office Mac controller and a home
Windows agent: three of four negotiations fell back with `punching timed out`,
while one pull went direct. A standalone probe on the same pair punched in
1.2 s and completed its QUIC handshake in 80 ms, but started QUIC only after
receiving a probe. This is the real-link finding addressed by R3-1 below.

## Review response

One adversarial review of the first draft (read-only, against this code and
quic-go v0.63.0). Verdict on dropping the inner TLS: sound, provided both pins
are verified during the QUIC handshake and no application data precedes
verification. Findings and what changed:

| # | finding | change |
|---|---|---|
| 1 | relay frame and direct attach could select different paths | controller is the only selector and sends exactly one signal; device arbiter; selection final; ack on direct |
| 2 | automatic retry could repeat a committed push | no retry; error states path and whether the outcome is unknown |
| 3 | pin checked after `Accept` would let any cert in | `VerifyConnection` on both sides in the handshake; transport helpers not reused |
| 4 | exclusions not enforceable from the file handlers | `directAllowed` computed in `handleSession`, passed down |
| 5 | long direct transfer vs relay reaping | direct phase independent of the relay session after selection |
| 6 | cancellation not carried onto QUIC | one operation context per side; Pull gains `CloseOnCancel` |
| 7 | pull had no size check and left partial files | temp file, size check, rename; device sends error on read failure |
| 8 | candidate lists could aim probes anywhere | count, size and address validation; bounded probe schedule |
| 9 | unauthenticated Initials reach the listener | stated; lifetime bound; Retry for unlisted sources |
| 10 | quic-go allows 100 streams by default | `MaxIncomingStreams` 1, no uni streams, one selected data stream |
| 11 | fallback bound ignored gathering time | budget re-derived; acceptance bound 3.5 s |
| 12 | STUN replies could arrive before the queue exists | synchronous enabling call before the first request |
| 13 | dual-stack sockets flaky | separate v4 and v6 sockets; explicit close |
| 14 | wire schema ambiguous; `direct_done` hit the unknown-kind path | schema frozen above; `direct_done` removed |
| 15 | small pulls paid STUN and a socket | capability first, candidates only after the device offers |

Second round, on the revision above. Its verdict was not to implement until the
relay read ownership, revocation, active-transfer limits and pull semantics
were settled; they are now the "State rules" section.

| # | finding | change |
|---|---|---|
| R2-1 | no single owner of the relay read across hold, fallback and attach | one-shot read ownership, handed back to the request loop like `doExec` |
| R2-2 | direct transfers outlive token revocation; relay-reap description wrong | revocation is per operation, as on WebSocket today; description corrected |
| R2-3 | hold cap did not cover running transfers; no progress timeout | `directMaxActive` covers both; 30 s without file bytes aborts |
| R2-4 | temp-file pull changed existing overwrite semantics and is not atomic on Windows | dropped; pull writes in place as today and checks the size |
| R2-5 | 8-slot cap could crowd out the LAN address | default-route addresses first, fixed order |
| R2-6 | `ok` could precede worker registration | register with the lifecycle first, then `ok` |
| R2-7 | Retry exemption described as verification | described as an optimisation; IP-only match defined |
| R2-8 | device could answer with no candidates | no candidates, no `direct` |
| R2-9 | Android IPv6 enable condition not closed | default-route probe only; ULA-only Android uses IPv4 |

Third round, after a real office-to-home run and an independent adversarial
code review:

| # | finding | change |
|---|---|---|
| R3-1 | Initials sent before the device punched were dropped; the next PTO often missed the 2.5 s budget | consume incoming probes and start one additional dial per validated source address, capped at 8 |
| R3-2 | unsolicited `DirectInfo` bypassed controller `WANCTL_DIRECT=0` | send `direct_fallback` and keep relay data when the request did not advertise direct |
| R3-3 | trickled JSON control bytes reset the no-progress clock | reset only after push file bytes are written or pull file bytes are sent |
| R3-4 | a completed transfer held a slot and file until QUIC connection close | release the file and slot when the data phase ends; close the connection separately |
| R3-5 | concurrent candidate dials could complete in a different order at the device | accept up to 8 authenticated connections during hold; first `direct_attach` wins and closes the rest |
| R3-6 | no-answer diagnostic changed old-peer output | print no new line when there is no `DirectInfo`; print a reason only after a direct attempt |

Fourth round, a real run from a controller behind a symmetric NAT to the same
home device, with both ends traced:

| # | finding | change |
|---|---|---|
| R4-1 | the device advertised its Tailscale addresses; with the controller's tailnet path on DERP, the handshake won and a 64 MB pull ran at about 20 KB/s | overlay addresses are never candidates, and are dropped from a peer's list one by one |
| R4-2 | a push that fell back failed with "direct hold timed out": the fallback shared an HTTP upload with the first file bytes and took longer than the hold to arrive | the controller flushes the carrier after writing the fallback; a test with a throttled relay reproduces the failure |
| R4-3 | Ctrl-C killed the CLI outright, so a pull never said the local file was incomplete | push and pull cancel their context on SIGINT/SIGTERM and say they were cancelled |
