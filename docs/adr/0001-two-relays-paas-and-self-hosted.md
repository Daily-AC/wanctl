# ADR 0001 — Production stays on the managed relay; the self-hosted VPS is an opt-in fast lane

Status: accepted (2026-07-29)

## Context

wanctl's production relay runs on a hosting PaaS at `relay.example.com`.
That platform's edge nginx strips `Connection: Upgrade` and buffers streaming
responses, so the WebSocket carrier cannot be used through it; the HTTP
long-poll carrier in `internal/httpconn` exists precisely to survive that edge.

At one point the whole backend was migrated to a self-managed VPS
(`192.0.2.10`) to get WebSocket back: relay + nginx with upgrade headers
preserved, a full copy of the Postgres database, five-platform binaries, and
the code defaults (`DefaultRelay`, `DefaultTransport`) flipped to the raw IP
with `ws`. The portal was redeployed pointing at it.

That was reverted a few days later ("fix: use production relay defaults") when
v0.1.0 was published: the VPS has no registered domain — it can only be
reached by raw IP with an IP certificate — and a raw IP is not something we
can hand out as *the* install endpoint, especially as the trust bootstrap for
`install.ps1`. The revert carried no explanation beyond a one-line commit
subject, and for the next several days the VPS kept running a full, unused,
publicly reachable copy of the relay that nobody had written down anywhere.

## Decision

Keep **exactly one production entry point**: `relay.example.com` with HTTP
long-poll. It owns the database, issues and revokes tokens, serves installers
and `/skills`, and is what every default in the code points at.

Keep the VPS running as an **opt-in second relay** for WebSocket, selected per
invocation via `WANCTL_RELAY` + `WANCTL_TRANSPORT=ws`. It resolves tokens
against production over `/admin/tokens/resolve` instead of holding its own
database.

## Alternatives rejected

**Decommission the VPS relay entirely.** Simplest, and it was the
recommendation until measurement changed the picture: 8 MB pushes complete in
~2 s over WebSocket versus ~6–10 s over long-poll, a 4–5× difference that a
`push`-heavy workflow feels immediately. Throwing that away to save one
systemd unit was the wrong trade.

**Promote the VPS to production.** Rejected for the same reason the earlier
migration was reverted: no registered domain, so a raw-IP endpoint would
become the published install and trust-bootstrap URL. IP certificates also
renew on a much shorter cycle, which is a worse dependency for the one
endpoint that must never be down.

**Give the VPS its own copy of the token database** (what the earlier
migration actually did). Rejected: it silently forks credentials. A token
revoked on the managed relay stayed valid on the VPS — that was live for
several days and nobody would have noticed, because nothing was reading the
VPS's audit table either.

## Consequences

- Defaults in `internal/config` continue to point at the managed relay. Anyone
  wanting the fast lane sets two environment variables, plus `NO_PROXY` for
  the raw IP.
- The VPS has no ACL table, so **cross-namespace shared devices cannot be
  dialled through it**. Same-namespace dials are unaffected. Accepted rather
  than replicating the credential store.
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
- A couple of old canary/test devices are still in the production device table
  and may be stale.
