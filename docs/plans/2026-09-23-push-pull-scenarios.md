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
