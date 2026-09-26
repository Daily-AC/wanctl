# Large push and pull over several TCP connections, 2026-09-26

## Why

Since 2026-09-26 the relay runs on a Hong Kong host (tchk) behind nginx,
HTTP/2 only. Measured that day between the owner's Beijing home line and tchk:

- One TCP connection is the ceiling, not the host or the line. `curl` with one
  connection downloads from tchk at about 5.6 MB/s; four parallel HTTPS
  downloads reached 27.6 MB/s at night and 18.7 MB/s by day (tchk's 200 Mbps
  cap). Round trip 53 ms, loss 3 % by day and 9–11 % at the evening peak.
  Uploads are worse per connection (ssh 3.4–10 MB/s) because the sender is a
  Mac or a Windows box without BBR.
- A 30 MB push from the Mac to the 5090 takes 8.2–9.1 s (about 3.5 MB/s) and
  a pull 9.9–15.4 s (about 2.3 MB/s). Both are one TCP connection per leg.

Every leg of a transfer crosses this link: push is controller upload plus
device download, pull is device upload plus controller download. All four
legs have to spread over several connections, or the slowest one stays the
cap.

## Where one connection is forced today

1. **One transport per process.** `httpconn.defaultClient`
   (`internal/httpconn/httpconn.go:185`) uses `relayhttp.Shared()`
   (`internal/relayhttp/relayhttp.go:78`), a single `http.Transport` cloned from
   the default with HTTP/2 on (`relayhttp.go:83`). Go's HTTP/2 client keeps one
   connection per host and multiplexes every request onto it; it opens another
   only when the server's concurrent-stream limit (nginx: 128) is used up. So
   both directions of every session, the agent's `/h/poll` loop and all
   concurrent sessions share one TCP connection.
2. **The upload window is already parallel, but on that one connection.**
   `upWindow = 4` (`httpconn.go:93`) keeps four sequenced `/h/up` in flight
   (`sendLocked`, `httpconn.go:392`); the relay reorders them
   (`sideQueue.pushSeq`, `internal/relay/http.go:86`, window `maxSeqAhead = 64`,
   `http.go:81`). Nothing on the relay side needs to change for uploads.
3. **The download is serial end to end.** The reader issues one `/h/down` at a
   time from `Read` (`httpconn.go:193–310`), acknowledging the last chunk with
   `ack=` and growing `max=` from 2 to 16 MiB (`httpconn.go:137–139`, `:313`).
   The relay admits one poll per direction (`sideQueue.turn`, `http.go:41`) and
   holds exactly one unacknowledged chunk (`unacked`, `http.go:56`;
   `takeUpTo`, `http.go:236`). A second connection would have nothing to carry.

## Design

### Lanes

A process keeps up to `N = 4` relay transports ("lanes"). Lane 0 is today's
`relayhttp.Shared()`. Lanes 1–3 are further `relayhttp.Transport` values built
the same way (`relayhttp.New(nil)`), created on first use and shared by every
session in the process. Each is its own connection pool, so each holds its own
TCP connection (or its own QUIC connection where HTTP/3 is in use). httpconn
chooses the lane per request; nothing else in the code sees them.

Lane choice is what keeps small commands exactly as they are: a request takes
the **lowest lane with no request of this session and direction in flight**.
A command that never has two requests in flight in one direction only ever
uses lane 0 and opens no extra connection. Only a bulk transfer, which fills
the window, reaches lanes 1–3.

`N = 4` because four connections already reached tchk's 200 Mbps cap in the
measurement above; more would only take a larger share of a relay other users
share, and hold more relay memory. It is a constant, not a setting.

### Upload (controller in push, device in pull)

Client-side only. `upWindow` becomes `2 × N = 8`, so each lane has one `/h/up`
waiting for its answer while the next is sending; every post goes out on the
lane chosen above. Sequencing, relay reorder, retry-by-sequence and the
fallback to one-at-a-time against a relay without `X-Wanctl-Up-Seq` stay as
they are. A failed post is retried on the next free lane; the relay drops the
duplicate by sequence, as today.

### Download (device in push, controller in pull)

The reader asks for chunks by number and keeps up to `N` polls in flight, one
per lane, reassembling in order before `Read` returns bytes.

Wire change, all optional:

- The relay announces `X-Wanctl-Down-Window: 4` wherever it already announces
  `X-Wanctl-Up-Seq` (`/h/poll` job, `/h/dial`, `/h/up`) and on every `/h/down`
  answer. A reader that has not seen it never sends the new parameter.
- A windowed poll is `GET /h/down?session&role&ack=A&want=K&max=M` with
  `A < K ≤ A + 4`. `ack` stays cumulative: the highest sequence below which the
  reader holds every chunk.
- The relay keeps a small map of assigned chunks per direction instead of one
  `unacked` slot, dropping every chunk `≤ A` on each poll:
  - chunk `K` already assigned: return it again, same bytes, same sequence;
  - `K` is the next unassigned number: drain up to `min(M, 4 MiB)` within the
    usual poll wait and assign it `K` (204 if nothing arrived);
  - `K` further ahead: wait, within the same poll wait, until `K − 1` is
    assigned, then as above; otherwise 204;
  - `K > A + 4` or `K ≤ A`: 400, which a correct reader never triggers.
  Assignment stays serialised (today's `turn` guards assignment only); returning
  an already assigned chunk does not wait for it.
- A poll without `want` is served exactly as today, which is the same map with
  one entry: the lowest assigned chunk above `ack`, else the next one. That
  keeps pre-window readers and the in-process bridge (`pollDrain`) unchanged.

Reader side: `Read` keeps today's single-poll loop until a chunk comes back
full (`len == max`), which is the evidence of a backlog. From then a small
prefetcher keeps polls for `ack+1 … ack+4` in flight, one per lane; it drops
back to one poll when chunks come back short or a poll returns 204. A failed
or truncated poll is re-asked for the same `K` on the next free lane; the relay
still holds it. Chunk size while windowed is capped at 4 MiB (the reader's `max`
still adapts downward on a slow lane), so a direction holds at most 16 MiB on
the relay and 16 MiB in the reader.

`settled` and `undelivered` (`http.go:179`, `:192`) must count the map, not the
single slot, or a session could be retired with chunks still unacknowledged.

### Compatibility

| relay | controller | device | result |
|---|---|---|---|
| new | new | new | push and pull use four lanes on every leg |
| new | new | v0.13.0 | controller legs use lanes (push upload, pull download); device legs stay single |
| new | v0.13.0 | new | device legs use lanes; controller legs single |
| v0.13.0 | new | new | uploads use lanes (the old relay already reorders); downloads stay single because no window is announced |

No combination is slower than today, and no combination can put bytes out of
order: uploads are ordered by the relay as now, windowed downloads only run
after the relay said it supports them.

### HTTP/2, HTTP/1.1, nginx

Lanes are separate transports, so the outcome does not depend on how Go pools
HTTP/2 or HTTP/1.1 connections. nginx on tchk sets no `limit_conn`; the 09-26
`http2_body_preread_size 1m` fix applies to each lane. HTTP/3 stays off on tchk;
where a relay does serve HTTP/3 each lane is its own QUIC connection, which is
equally fine. A configured HTTPS proxy sees four CONNECTs instead of one.

### One connection dying mid-transfer

Nothing is lost, because every byte in flight is identified by sequence on both
directions: an upload is re-posted with the same `seq` and deduplicated; a
download is re-asked with the same `want` and the relay re-serves it until the
cumulative `ack` passes it. The lane whose request failed is closed
(`CloseIdleConnections`) and redials on next use. Existing bounds stay: four
attempts per upload, six consecutive failures per poll.

## Deliberately left out

- Splitting a file at the application layer into several sessions: several
  handshakes, several approvals, and a partial-file assembly protocol, for the
  same bytes the carrier can stripe.
- A user setting for `N`, a threshold flag, or a per-command switch.
- The WebSocket carrier and the in-process bridge; the default carrier is
  `http`, and WebSocket is one TCP connection by construction.
- Any change to the direct lane (`feat/direct-lane`) or to HTTP/3.
- One long streaming response per lane (measured 2026-09-24 to collapse on the
  old CDN path; windowed polls with `Content-Length` keep today's behaviour).

## Acceptance (written before implementation)

All runs Mac controller → 5090 device through production tchk, daytime, Mac
without proxy variables (`env -u https_proxy -u HTTPS_PROXY -u all_proxy
-u ALL_PROXY`). Every transfer's SHA-256 equals the source.

1. **Throughput.** 256 MB push ≥ 10 MB/s and 256 MB pull ≥ 7 MB/s, median of
   three, measured as file size over wall time of the CLI command. Both ends
   share the home line, so the ceiling is its upload to tchk: measure that
   first with four parallel HTTPS uploads; if it is below 13 MB/s, the bar is
   75 % of it for push and 55 % for pull instead.
2. **30 MB**: push ≤ 5 s and pull ≤ 6 s, median of three (today 8.2–9.1 s and
   9.9–15.4 s).
3. **All four legs striped.** During a 256 MB push and a 256 MB pull, the CLI
   process on the Mac (`lsof -a -i TCP -p <pid>`) and the agent process on the
   5090 (`Get-NetTCPConnection -OwningProcess <pid>`) each hold 4 established
   connections to the relay. (Both machines leave home through the same
   address, so counting on tchk cannot tell them apart.)
4. **Small commands unchanged.** 1-byte push and 64 KiB pull, 8 runs each,
   alternated with the v0.13.0 CLI and agent: median not worse by more than
   150 ms, and during each run the CLI process holds exactly one TCP connection
   to the relay.
5. **Mixed versions.** The four rows of the compatibility table, each with a
   30 MB push and pull: hashes equal, time no worse than the v0.13.0 pair. The
   old-relay row runs against a local relay built from v0.13.0 in the e2e
   harness.
6. **Lost connection.** During a 256 MB push and a 256 MB pull, kill one of
   the lane connections on tchk (`ss -K` on one of the four sockets): the
   transfer completes and the hash matches. In the test suite, a fault-injecting
   round tripper drops a response on lane 2 and truncates a body on lane 3, for
   both directions, and the stream arrives intact.
7. **Relay memory.** A unit test holds a direction at the window with 4 MiB
   chunks and proves the relay holds no more than 4 chunks and refuses
   `want > ack + 4`.
8. `go test -race ./...` and the repository's CI script pass.

## Implementation notes

- `sideQueue.assigned` is the authoritative download state. The old `unacked`
  field remains as a mirror of its oldest entry because existing package tests
  inspect it directly; replay, retirement, and retention decisions use the map.
- `DialWith` keeps an explicitly supplied HTTP client on every lane so existing
  fault-injection callers retain control of requests. Production `Dial` uses
  four process-shared transports, with lanes 1–3 created lazily.
