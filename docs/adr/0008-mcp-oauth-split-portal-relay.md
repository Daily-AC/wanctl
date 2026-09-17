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
`WANCTL_MCP_SEED`, the format `internal/mcp/rebind.go` already uses, under a
different prefix (`woa1.`) and audience. So the MCP server opens a bearer and
has the namespace and the relay token in hand with no database lookup and no
new key to rotate. Rotating the seed still invalidates every credential of
every kind at once, which is what the deployment docs already promise.

The stored half is sealed too (`wog1.`, beside the refresh token), so a copy of
the database is not a copy of anyone's device access: the seed lives in the
relay's environment, never in Postgres.

**No bearer keeps the old path.** A request with no `Authorization` header
behaves exactly as before. Clients that hold their session open never meet any
of this.

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
