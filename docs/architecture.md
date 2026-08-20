# wanctl — Architecture

Control another device across the public internet from a terminal (or an AI
agent's shell), over an end-to-end-encrypted, relayed channel. A web portal
issues tokens and manages sharing; a Postgres-backed relay authenticates and
brokers; devices enforce a local, approval-based permission policy.

```
controller (you/agent) ──┐                            ┌── device (wanctl agent)
   wanctl exec/push/logs  │   relay (public broker)   │     policy engine + approval
                          ├── byte-pipe + registry ───┤     JSONL event log
                          │   token auth + ACL + audit│
   E2E mutual-TLS ════════╪════════ over the pipe ════╪════ (relay sees only ciphertext)
                          │                           │
   portal (web, SSO) ─────┘  issues tokens, ACL ──────┘  (thin proxy to relay /admin/*)
```

## Components

One Go binary; the role is chosen at runtime:

- `wanctl relay` — the public broker. Postgres-backed (`DATABASE_URL`):
  hashed-token resolve, cross-namespace ACL, metadata audit, device registry.
  Serves the signed-manifest `/dl/<artifact>` distribution and the `/skills`
  document for AI controllers. Falls back to a static `WANCTL_TOKENS` env map
  when no database is attached. The secret-gated admin API `/admin/*` backs the
  portal.
- `wanctl portal` — the web app. **No database of its own**: a thin proxy to
  the relay's `/admin/*` using a shared `WANCTL_ADMIN_SECRET`, scoped to the
  namespace resolved from the authenticated identity. The portal is also a
  first-class controller: it holds its own Ed25519 identity and a privileged
  dial token, and drives a live device console over the same E2E tunnel.
- `wanctl agent` — the controlled device. Outbound-only to the relay; each
  session does a server-side TLS handshake, TOFU authorization, then
  policy-gated exec/file serving. Writes a JSONL event log.
- `wanctl exec|push|pull|peers|id|trust|rules|logs|net|docs|update` —
  controller commands.

Package map: `transport` (Ed25519 identity, mutual TLS, TOFU), `protocol`
(framed wire format), `wsconn` (WebSocket↔net.Conn), `httpconn` (HTTP
long-poll↔net.Conn — the proxy-agnostic carrier), `relay` (broker + http
transport + pgstore + admin + dist), `agent`, `client`, `server` (shell+files),
`policy` (rules+approver), `console` (transport-neutral approval queue),
`portal`, `eventlog`, `sessionauth` (relay-issued capability grants).

## Transports

Two carriers speak the **same** TLS + framed protocol:

- **WebSocket** (`wsconn`) — for deployments whose ingress preserves WebSocket
  upgrades (direct exposure, or a reverse proxy configured for upgrades).
- **HTTP long-poll** (`httpconn`) — the proxy-agnostic default:
  `WANCTL_TRANSPORT=http`. Up-channel is one POST per write; down-channel is a
  long-poll GET that returns available bytes / 204 retry / 410 EOF. Every
  response is finite, which survives edges that strip `Connection: Upgrade`
  **and** buffer streaming responses (a plain streaming GET deadlocks behind
  such an edge — this was measured, not theorized).

Measured on an 8 MB payload, WebSocket buys 4–5× push and ~2× pull throughput;
interactive exec latency and line-by-line streaming spacing are equivalent on
long-poll. Pick WebSocket for file-transfer-heavy fleets, long-poll for
maximum ingress compatibility.

## Trust model

Three independent layers; compromising one does not collapse the others:

1. **Relay admission** — a bearer token (`wanctl_<40 hex>`) maps to a
   namespace. Tokens are stored as SHA-256 hashes, may carry labels and
   expiry, and are revocable. The relay authorizes *dials* by namespace and
   ACL; it cannot see inside sessions.
2. **End-to-end identity** — controller and device each hold a self-signed
   Ed25519 certificate; sessions run mutual TLS 1.3 *over* the relayed pipe
   with fingerprint pinning (TOFU on both sides). A relay or database
   compromise leaks metadata only. Devices additionally refuse pairing prompts
   from controllers that do not identify themselves with a label.
3. **Device-local policy** — a rules engine gated by human approval
   (Claude-Code-style: yes / always-this-dir / always-global / no). Command
   rules match a single simple command only — anything with shell operators,
   substitutions, or redirects requires an exact match. File rules bind the
   actual opens to a directory root (no symlink escape). `bypass` mode
   auto-allows but still logs; elevated exec is deliberately *not* covered by
   bypass or ordinary exec rules. Headless with no approver subscribed means
   deny, not hang.

Cross-namespace sharing is a relay-side ACL grant `(owner, device, grantee,
perms)`. Grants are capability-bounded at the protocol layer: a shared grantee
can at most receive exec/read/write — console and logs capabilities cannot be
expressed in a grant. The relay stamps each session with its capabilities over
the authenticated agent control channel, and the device enforces them again
per request. Shared devices are read-only in the portal: approvals, rules,
mode, and unbinding stay with the owner.

## Remote device console

The portal opens a live console to a device over the E2E tunnel: pending
approvals, rules, mode, and the activity log, pushed in real time
(`KindApprovalNotif` carries the full console state). Approval decisions from
a local TTY and from the portal feed one queue; first answer wins. The device
pre-trusts portal identities through an explicit `portal_admins` set, seeded
at enrollment and manageable with `wanctl portal-admins add|list|remove`
(removing the last one is refused). Rotating a portal identity is an overlap
migration: seed old+new, deploy, verify, then remove old.

Because the platform edge may buffer streams, the portal's browser-facing
event channel is a 25-second long-poll rather than SSE.

## Server logs

Relay and portal tee process logs to a bounded in-memory ring (2,000 lines /
2 MiB) and expose the secret-gated `GET /admin/logs`. The portal serves its own
ring and proxies the relay's, so it is the single entry point:

```bash
WANCTL_ADMIN_SECRET=... wanctl logs --service portal
WANCTL_ADMIN_SECRET=... wanctl logs --service relay --since 30m --grep <term>
```

Both services redact token/secret/password/API-key labelled values, bearer
credentials, and base64-like strings ≥ 32 chars *before* applying `grep`, so a
secret passed as a grep term cannot confirm its own presence. `--follow` is
deliberately unsupported; the command errors instead of pretending to stream.

## Release distribution

`/dl` is an allowlist, not a directory listing: the relay serves exactly the
artifacts named by an offline-signed manifest (Ed25519), verified again on
every read; anything else 404s. Installers are the trust-bootstrap exception —
they are served unsigned by necessity, so prefer fetching them from the
project's release page for machines that matter, and compare SHA-256
out-of-band. Install scripts verify an RSA signature instead of Ed25519
because a stock macOS LibreSSL and PowerShell 5.1 cannot verify Ed25519 — see
`docs/release-signing.md`.

`wanctl update` verifies the manifest signature, swaps the binary, and — on
platforms where a supervisor may own the running agent under another account —
checks whether it can actually terminate the old process instead of reporting
success while the old version keeps serving. After any upgrade, trust the
running process's start time against the binary's mtime, not the version on
disk.

## LAN fast path (optional)

Devices can keep a second uplink to an intranet WebSocket relay
(`WANCTL_LAN_RELAY`; unset disables). Controllers switch with
`wanctl net wan|lan|auto|status` (persisted; `auto` probes the LAN relay's
`/healthz`; an explicit `WANCTL_RELAY` always wins). LAN dials bypass
`HTTP(S)_PROXY` env, because corporate proxies blackhole private ranges. A LAN
relay without a database can resolve tokens against the main relay
(`WANCTL_UPSTREAM_RELAY` + `WANCTL_ADMIN_SECRET`, 5-minute cache), so
portal-issued tokens work everywhere. A `ws://` LAN relay is acceptable only
inside an encrypted overlay (WireGuard or similar); otherwise use `wss://`.

## Field notes (hard-won, don't relearn)

- **PaaS edges lie twice**: the same edge that strips WebSocket upgrades will
  also buffer streaming responses and ignore `X-Accel-Buffering`. Long-poll
  (every response finite) is the shape that survives both.
- **`/healthz` may be shadowed** by a platform edge that answers it with its
  own JSON; probe a real application route to test reachability.
- **When a platform hides a secret** (masked `DATABASE_URL`, write-only env
  editors), change the architecture instead of fishing for it — the
  portal-as-proxy design exists because the portal could not be given the DSN.
- **The SSO identity header is an assertion from the proxy.** The proxy must
  strip any client-supplied value for that header; the app cannot authenticate
  it by itself.
- **Detaching a process over ssh**: `setsid CMD & exit 0` sometimes dies. Use
  `nohup setsid CMD >log 2>&1 </dev/null & disown`.
- **Passwordless sudo changes installer semantics**: a root-owned
  `/usr/local/bin/wanctl` from an earlier sudo install makes a later non-sudo
  reinstall silently fall back to `~/.local/bin`. Pin with `WANCTL_BIN`.
- **A retry loop that logs nothing produces the least debuggable failure there
  is.** The agent's poll loop logs the first failure, then ~one per minute,
  then the recovery. Apply the same rule to any loop that retries forever.
- **A "verified on the device" claim is only worth the invocation form it
  used.** Verify a new platform by running the published installer and then
  the bare command off PATH — not a hand-copied binary.
- **Android breaks four Unix assumptions** (resolver, HOME, /bin/sh, traversal
  permissions) and Termux breaks two more (linker argv duplication, no exec
  from an app's private dir). The full account, including why the APK's
  `lib/<abi>/` directory is the one place an app may exec from, lives in
  `docs/android.md`.

## Local smoke test (no external services)

```bash
go build -o /tmp/wanctl .
WANCTL_TOKENS="tk1:teamA" /tmp/wanctl relay --addr :18080 &
WANCTL_CONFIG_DIR=/tmp/wc-agent /tmp/wanctl agent --relay ws://127.0.0.1:18080 --token tk1 --name lab-pc --yes &
WANCTL_CONFIG_DIR=/tmp/wc-ctl WANCTL_RELAY=ws://127.0.0.1:18080 WANCTL_TOKEN=tk1 /tmp/wanctl peers
WANCTL_CONFIG_DIR=/tmp/wc-ctl WANCTL_RELAY=ws://127.0.0.1:18080 WANCTL_TOKEN=tk1 /tmp/wanctl exec --target lab-pc "echo ok"
```
