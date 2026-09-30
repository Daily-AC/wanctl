# 0008 — MCP OAuth: consent on the portal, tokens on the relay

Date: 2026-09-17
Status: accepted

## Context

The hosted MCP endpoint stores a login in memory keyed by `Mcp-Session-Id`
(`internal/mcp/server.go`, `sessionStore.get`). That works for a client that
holds one session open for a conversation, which Claude Code, Codex and Cursor
all do.

ChatGPT's MCP client does not. It re-sends `initialize` and takes a new
`Mcp-Session-Id` for **every tool call** (community.openai.com 1377210 and
1364975, openai-apps-sdk-examples#165). Under session-keyed login that means a
successful `wanctl_login` is followed immediately by `LOGIN REQUIRED` on the
next call, forever. The `rebind` credential does not save it either: the model
would have to volunteer it on every single call.

The MCP authorization specification answers this by moving identity off the
session: the client gets an OAuth 2.1 access token and attaches it to every
request. claude.ai's remote-MCP support uses the same path.

## Decision

Implement the MCP authorization spec, split across the two services we already
run, and keep the existing session path untouched.

**Machine endpoints on the relay.** Discovery
(`/.well-known/oauth-protected-resource`, `/.well-known/oauth-authorization-server`),
dynamic client registration, the token endpoint and revocation all live on the
relay. It is the service that has the Postgres, the MCP seed and a configured
public origin, and the origin is what the issuer and the resource identifier
have to be — derived from configuration, never from a request Host, because a
client compares those identifiers byte-for-byte across three documents.

**The consent page on the portal.** `/oauth/authorize` is a page a person
reads and a decision they make, and the portal is the only service that knows
how to turn a GitHub login into a wanctl namespace. Putting a login on the
relay would create a second thing that can mint a session, on the service whose
job is to broker bytes it cannot read. The portal talks to the relay over the
existing admin-secret channel, the same way `/enroll` already does.

**Access tokens reuse the rebind seal.** `internal/mcpauth` seals
`{ns, relay token, client_id, jti, exp}` with a key HKDF-derived from
`WANCTL_MCP_SEED`, the format `internal/mcp/rebind.go` (removed in v0.19.0) already uses, under a
different prefix (`woa1.`) and audience. So the MCP server opens a bearer and
has the namespace and the relay token in hand with no database lookup and no
new key to rotate. Rotating the seed still invalidates every credential of
every kind at once, which is what the deployment docs already promise.

The stored half is sealed too (`wog1.`, beside the refresh token), so a copy of
the database is not a copy of anyone's device access: the seed lives in the
relay's environment, never in Postgres.

**No bearer keeps the old path.** A request with no `Authorization` header
behaves exactly as before. Clients that hold their session open never meet any
of this. *(Superseded in v0.19.0: a request without a bearer gets 401; see the
amendment below.)*

## Consequences

- One relay token per authorization, labelled `oauth:<client name>`, visible
  and revocable in the portal's token list. Refreshing rotates the refresh
  token but keeps the same relay token, so a long-lived connector does not
  litter that list.
- A bearer request costs one token-store lookup, so revoking takes effect on
  the next call rather than when the hour-long access token expires.
- The pinned-server store for OAuth sessions is shared per namespace rather
  than per session. It has to be: a client that opens a new session per call
  would otherwise be asked to confirm the same device identity forever. It
  stays process-local, so a relay restart asks once more.
- Two tables (migration 010) and no new environment variables. `WANCTL_PORTAL`
  and `WANCTL_PUBLIC_ORIGIN` gain meaning on the relay; without either, or
  without a database, OAuth stays off and the endpoint is what it was.
  *(Superseded in v0.19.0: without them the endpoint is off and `/mcp`
  answers 503.)*

## Alternatives rejected

- **Key sessions by a client fingerprint instead of `Mcp-Session-Id`.** Cheap,
  and wrong: everything available to fingerprint on (User-Agent, source IP) is
  shared by every user of a hosted AI product, so two strangers would land in
  one logged-in session.
- **Make the model resend the rebind credential on every call.** Depends on a
  model choosing to do something on every turn, and puts a credential that
  carries a relay token into the conversation transcript repeatedly.
- **Run the whole OAuth server on the portal.** The portal has no MCP seed and
  the token endpoint is called by machines that never see the portal's cookie
  domain; the relay would then have to call back to the portal to verify every
  bearer.

## Amendment 2026-09-30 (v0.19.0): OAuth only

**Decision.** Delete the no-bearer path: the portal one-time code redeemed
through `wanctl_login`, the `wrb1.` rebind credentials, their in-memory
revocation, the per-session login state, and the per-address budget for
sessions nobody had logged in to. The hosted endpoint no longer registers
`wanctl_login`, and its `instructions` no longer mention logging in. A request
without `Authorization` gets 401 with
`WWW-Authenticate: Bearer resource_metadata="<origin>/.well-known/oauth-protected-resource"`,
the same challenge an invalid bearer already got, so a client starts
discovery. A relay without the OAuth prerequisites (database,
`WANCTL_PUBLIC_ORIGIN`, `WANCTL_PORTAL`) logs why and answers 503 at `/mcp`.
The standalone `wanctl mcp --http` mode and the container role
`WANCTL_ROLE=mcp` are removed: that login was the only one they had. The local
stdio `wanctl mcp`, and its `wanctl_login`, are unchanged.

**Why.** Security finding CX-04: logging out revoked a rebind credential only
in the relay process's memory, so after a relay restart a logged-out
credential worked again for the rest of its seven days. Making that revocation
durable would mean a second credential store next to the OAuth grants, for a
path nobody uses.

**Evidence.** nginx logs for 2026-09-25..09-29 show a single request to
`/mcp`, and it was our own check. The web AIs that need a
hosted endpoint (ChatGPT, claude.ai) connect with OAuth; AI hosts on the
user's own machine use stdio `wanctl mcp`.

**Consequences.** A third-party hosted AI without OAuth support cannot use
the public endpoint; it has to run the local stdio server instead. A relay
that served `/mcp` without a portal stops serving sessions on upgrade. Saved
`wrb1.` credentials stop working, with nothing to clean up.
