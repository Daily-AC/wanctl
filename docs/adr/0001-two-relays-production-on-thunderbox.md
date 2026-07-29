# ADR 0001 — Production stays on thunderbox; the `ls` relay is an opt-in fast lane

Status: accepted (2026-07-29)

## Context

wanctl's production relay runs on thunderbox at `wanctl-relay.***REMOVED***.***REMOVED***.com`.
That platform's edge nginx strips `Connection: Upgrade` and buffers streaming
responses, so the WebSocket carrier cannot be used through it; the HTTP
long-poll carrier in `internal/httpconn` exists precisely to survive that edge.

On 2026-07-22 the whole backend was migrated to a company Tencent Cloud box
(`ls`, `***REMOVED-IP***`) to get WebSocket back: relay + nginx with upgrade
headers preserved, a full copy of the Postgres database, five-platform binaries,
and the code defaults (`DefaultRelay`, `DefaultTransport`) flipped to the raw IP
with `ws`. The portal was redeployed pointing at it.

On 2026-07-24 all of that was reverted in `feadf09` ("fix: use production relay
defaults") when v0.1.0 was published: `ls` has no ICP-registered domain — it can
only be reached by raw IP with an IP certificate — and a raw IP is not something
we can hand to colleagues as *the* install endpoint, especially as the trust
bootstrap for `install.ps1`. The revert carried no explanation beyond a one-line
commit subject, and for the next five days `ls` kept running a full, unused,
publicly reachable copy of the relay that nobody had written down anywhere.

## Decision

Keep **exactly one production entry point**: `wanctl-relay.***REMOVED***.***REMOVED***.com`
with HTTP long-poll. It owns the database, issues and revokes tokens, serves
installers and `/skills`, and is what every default in the code points at.

Keep `ls` running as an **opt-in second relay** for WebSocket, selected per
invocation via `WANCTL_RELAY` + `WANCTL_TRANSPORT=ws`. It resolves tokens
against production over `/admin/tokens/resolve` instead of holding its own
database.

## Alternatives rejected

**Decommission `ls` entirely.** Simplest, and it was the recommendation until
measurement changed the picture: 8 MB pushes complete in ~2 s over WebSocket
versus ~6–10 s over long-poll, a 4–5× difference that a `push`-heavy workflow
feels immediately. Throwing that away to save one systemd unit was the wrong
trade.

**Promote `ls` to production.** Rejected for the same reason 07-24 rejected it:
no registered domain, so a raw-IP endpoint would become the published install
and trust-bootstrap URL. IP certificates also renew on a much shorter cycle,
which is a worse dependency for the one endpoint that must never be down.

**Give `ls` its own copy of the token database** (what the 07-22 migration
actually did). Rejected: it silently forks credentials. A token revoked in the
portal stayed valid on `ls` — that was live from 07-22 until 07-29, and nobody
would have noticed, because nothing was reading `ls`'s audit table either.

## Consequences

- Defaults in `internal/config` continue to point at thunderbox. Anyone wanting
  the fast lane sets two environment variables, plus `NO_PROXY` for the raw IP.
- `ls` has no ACL table, so **cross-namespace shared devices cannot be dialled
  through it**. Same-namespace dials are unaffected. Accepted rather than
  replicating the credential store.
- No audit rows for traffic that takes the fast lane. Acceptable while it is a
  personal-throughput tool; if it ever carries team traffic this must be
  revisited, because the audit trail is a stated property of the system.
- If production is down, the fast lane goes down with it within the 5 minute
  token cache. That is intentional: one place issues and revokes credentials.

## Follow-ups

- The v0.1.0 binary still ships the raw-IP/`ws` defaults from the migration
  window. `docs/releases/v0.1.0.md` tells those users to set
  `WANCTL_RELAY`/`WANCTL_TRANSPORT` explicitly; a v0.1.3 build would remove the
  footgun.
- `zyl-v010-canary` and `LAPTOP-SA1VSRD4` are still in the production device
  table and may be stale.
