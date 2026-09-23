# Push and pull scenarios before the transport release, 2026-09-23

## Result

Every mainland device measured so far gets 20–190 KB/s per TLS-over-TCP
connection to the relay's Cloudflare edge, and 12–42 MB/s over plain HTTP on the
same edge. HTTP/3 was measured on one of them and is not shaped (9–42 MB/s).
Three consequences shape the release:

1. Every leg a mainland node opens to the relay is shaped, in both directions,
   whether the node is a controller or a device.
2. Auto-update is a download over the same shaped path. A 20 MB binary at
   31–55 KB/s takes 6–11 minutes, and the updater gives up after 5. Every
   mainland-direct agent is behind (v0.9.2, v0.10.0, v0.11.0) while the one
   behind a proxy is current (v0.12.1). The old agents cannot be fixed by new
   agent code, because they are running old code.
3. WebSocket would not help: the CDN carries it over TCP.

## How a transfer moves

A session is two queues on the relay. Each side writes with `POST /h/up` and
reads with `GET /h/down`.

| | sender's leg | relay | receiver's leg |
|---|---|---|---|
| push | controller → edge → tunnel → relay (upload) | queue | relay → tunnel → edge → device (download) |
| pull | device → edge → tunnel → relay (upload) | queue | relay → tunnel → edge → controller (download) |

Each leg is capped by the slower of its bandwidth and its round trips: uploads
send one batch (1 MiB after #122) per round trip, downloads take up to 2 MiB per
poll. Before any byte moves, a command pays about five end-to-end round trips
(resolve, dial, two TLS flights, hello) plus the operation's own.

## Who is on each end

| end | kinds | leg to the relay |
|---|---|---|
| controller | CLI or local MCP on a laptop | through the edge |
| controller | hosted MCP and portal | inside the VM, `http://127.0.0.1:8080`, no edge |
| device | home Windows (5090, 客厅笔记本) | through the edge, China Mobile home line |
| device | mainland cloud server (BMS) | through the edge, China Mobile cloud |
| device | laptop behind Clash | through a proxy node |
| device | Android | unmeasured |

## Network classes

| class | TLS over TCP | plain HTTP | HTTP/3 | seen on |
|---|---|---|---|---|
| mainland direct | 20–190 KB/s per connection | 12–42 MB/s | 9–42 MB/s | 5090 (all three), 客厅笔记本 and BMS (TLS/HTTP only) |
| explicit proxy | fast, via node | – | not used (bypasses the proxy) | Mac with `HTTPS_PROXY` |
| TUN proxy | – | – | slower than TCP via proxy | Mac without proxy env: 8 MB push 48–55 s |
| UDP blocked | shaped if mainland | – | falls back to HTTP/2 | unit test only |
| overseas | fast | – | fast | hk VPS: 1.1 MB/s uncached |

The tunnel leg (VM ↔ edge through 专线 nodes) is not shaped: egress ≥ 10 MB/s.
Ingress was not isolated; from the 5090 a 1 MiB `/h/up` completes in ~1.2 s.

## Sizes

| size | examples | what dominates |
|---|---|---|
| < 1 MB | scripts, configs, screenshots | round trips: 4–6 s today |
| 1–100 MB | binaries, APKs, datasets, updates | per-leg throughput |
| 100 MB–1 GiB | models | throughput; 1 GiB is the hard limit, larger goes by scp |

## Measured with #122 (Mac controller via proxy, 5090 agent on a dev build)

| | before | after |
|---|---|---|
| 8 MB pull | 3.3 MB in 43 s | 21.7 s |
| 8 MB push | did not finish | 16.6–19.7 s |
| 1 byte push | 5.0–6.6 s | 4–6 s |

## Gaps, by what they block

| gap | who is affected | fix | needs |
|---|---|---|---|
| old agents cannot download the update | every mainland-direct device | serve `/dl` so an old updater finishes inside 5 minutes (gzip 7.6 MB) | relay change; verify an old agent really upgrades |
| updater and CLI `update` use a plain client | same, from the next release on | route update downloads through relayhttp, resume on failure | client change |
| upload is one batch per round trip | large pushes and pulls | several `/h/up` in flight with a sequence number | relay + both ends, negotiated |
| round trips before the first byte | small files | merge resolve into dial, don't wait on close, send hello with the request | client + relay |
| UDP passes but is worse than TCP (TUN proxies) | some proxied users | `WANCTL_HTTP3=0`; no automatic detection | – |
| Android, UDP-blocked networks, hosted MCP push | unknown | measure | devices |

## Release candidate, measured 2026-09-23 18:00–18:45

Relay on a build of this branch (`wanctl:h3test` on the VM), 5090 agent on a
dev build, Mac controller through its proxy node. Evening, link variable.

| criterion | target | measured |
|---|---|---|
| 8 MB push | ≤ 10 s | 8.0–8.6 s; 8.5–13.7 s in a worse hour |
| 8 MB pull | ≤ 10 s | 7.7–8.7 s; 10.3–21.2 s in a worse hour |
| 100 MB push | ≤ 60 s | 39–42 s; one run at 123 s in a worse hour |
| 100 MB pull | ≤ 60 s | 42 s |
| 1 byte push | ≤ 3 s | 2.4–3.4 s (median 2.6) |
| old agent updates itself | yes | BMS v0.10.0 → v0.12.1 at 17:44 unattended, via the edge's `/dl` gzip and plain-HTTP redirect for the Go updater |
| UDP blocked | falls back, works | 5090 with outbound UDP firewalled: exec, push, 8 MB pull all correct over TCP |

What limits it now:

- The receiving side downloads one poll at a time, at most 2 MiB each, so a
  leg tops out near 2 MiB per round trip (about 3 MB/s at 0.4 s). Push and
  pull both sit at 2.4–2.5 MB/s. Addressed below.
- Spreading concurrent uploads over separate connections was tried and
  measured no gain once the window was 4, so it was not kept. Widening the
  window to 8 or 16 did not help either (16 was slower).
- Android is not yet measured.

## Receiving side, measured 2026-09-23 21:40 – 09-24 01:00

The link from the 5090 to the edge swung between 0.1 and 7.6 MB/s within
minutes all evening, so every comparison below interleaves its variants and
reports medians or ranges from the same minutes.

### Request size decides it

A probe on the 5090 fetched the relay's `/dl` binary through the same HTTP/3
stack the agent uses, 20 MB per variant, five interleaved rounds:

| requests | median |
|---|---|
| 2 MiB, one at a time (the current down poll) | 0.60 MB/s |
| 2 MiB, one at a time, client QUIC stream window 8 MiB | 0.59 MB/s |
| 2 MiB, two in flight | 2.49 MB/s |
| 8 MiB, one at a time | 2.91 MB/s |
| 16 MiB, one at a time | 4.20 MB/s |

A bigger client window changes nothing, so it was not kept.

### A response of unknown length collapses on this path

A throwaway site behind the same tunnel served synthetic bodies, fetched
through the relay's edge IP in the same minutes:

| 16 MB body | speed |
|---|---|
| with Content-Length | 2.2–2.9 MB/s |
| without, 256 KB writes flushed | 0.18–0.19 MB/s |
| without, 32 KB writes flushed | 0.46–0.73 MB/s |

At 2 and 8 MB the two shapes were within noise; the collapse sets in part
way through a longer body. The relay's down poll never declared a length:
it called `WriteHeader` before `Write`, so Go sent it chunked.

This is why streaming the down direction (one long framed response per poll,
the git and rsync way the research recommended) was built and dropped: on the
real path it ran 0.3–0.9 MB/s where polling ran about 1 MB/s.

### What shipped

- Down-poll responses declare Content-Length.
- A reader asks for its chunk in `max=`: 2 MiB first, doubling while full
  chunks arrive inside 4 s, up to 16 MiB, halving after one that took over
  30 s. An older relay ignores `max=` and serves 2 MiB.
- A session holding an unacknowledged chunk is kept 10 minutes without a
  request instead of 60 s, so a slow reader still downloading a large chunk
  from a proxy's buffer does not lose it; one with a poll in progress is never
  reaped.
- The handshake with the device gives up after 60 s. A relay can hand a
  session to an agent that has just exited, and the controller used to wait
  for as long as it was let.

### Results (Mac controller on the VM's LAN, so the upload leg costs nothing)

30 MB push, agents swapped in blocks of three, both on the same relay build
(which declares Content-Length to the old agent too):

| 5090 agent | 9 runs | median |
|---|---|---|
| this branch | 7.1–12.2 s | 9.1 s |
| previous head (2 MiB polls) | 15.2–31.3 s | 28.3 s |

With the link at 7.6 MB/s the old agent still took 30 s: it is bound by 2 MiB
per round trip, not by the link.

Acceptance on this branch: 30 MB push 7.6 s, 100 MB push 14.9 s, both
hash-checked. 1-byte push shows no regression: new and old clients
interleaved at 2.9–4.1 s from the Mac and 3.5–4.9 s from the 5090.

### Still open

- **Pull is bound by the device's upload leg**: 30 MB 32.7 s, 100 MB 59.2 s
  (about 1–1.7 MB/s). An upload matrix from the 5090 (1 MiB × 4, 1 MiB, 4 MiB,
  4 MiB × 2, 8 MiB, 16 MiB) showed no variant consistently ahead, so request
  shape is not the lever there. The next candidate is the tunnel itself:
  cloudflared in http2 mode is an HTTP/2 server with the default 1 MiB
  per-connection receive window, which bounds uploads per tunnel connection
  to 1 MiB per round trip whatever the client does. Unverified.
- **A controller can still wait forever after the handshake** if its device
  vanishes mid-session: its own polls keep the session alive, so the relay
  never reaps it. Seen once, right after swapping the 5090's agent.
- Android is not yet measured.
