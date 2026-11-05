# 0021 — Scoped command token: a credential that can run one command on one device

Date: 2026-11-06
Status: proposed (implementation on `feat/scoped-token`; not released)

## Context

Every credential wanctl issues today is all-or-nothing. A full token
(`wanctl_<40 hex>`) carries its owner's whole namespace: every device, every
verb (exec, push, pull, logs, shell), plus account management through the
`/u/*` endpoints. The only narrower credential is the WebFetch delegation
(`wfd_`), which is derived from a browser ticket and approved per request by a
human in the portal; it limits the *device* and the *time*, not the *action*
("grants add no action matrix", ADR 0007 and `internal/delegation`).

That gap is a real obstacle. A phone agent, a cron job on a laptop, or a
webhook receiver often needs exactly one thing — "run the deploy script on box
A" — and today it must hold a token that can run anything on every device the
owner has. If that token leaks, or the program holding it is prompt-injected,
the blast radius is the owner's whole fleet. This is why Iris does not use
wanctl for its "ask the project" feature.

The owner asked for it directly: *a restricted-rights token*.

## Decision

**A new credential kind, the scoped command token.** It is bound at issue time
to exactly one thing: *one device* and *one literal command string*. It can
run that command on that device, byte for byte, and nothing else. Version one
has this one scope and no others (no wildcards, no argument templates, no file
verbs); more scopes can be added later behind the same field.

### Format and storage

- Token: `wcs_<40 hex>`. The distinct prefix, like `wfd_`, lets every legacy
  code path refuse it by prefix without a database lookup.
- Stored as a SHA-256 hash in the existing `tokens` table with
  `kind = 'scoped'`, so expiry, revocation, listing and the "disabled account"
  predicate come from the machinery that already exists and is already tested.
- Scope lives in a new table (migration 016):
  `scoped_tokens(token_id PK → tokens.id, namespace, device_id, device_fp, command)`.
  `device_fp` is the device's fingerprint at issue time; if the device is later
  re-enrolled under a new identity the token stops resolving, exactly like a
  delegation grant (`delegation_devices`).
- The raw token is returned once at issue and never stored.

### Issue, list, revoke

- CLI, authenticated with the owner's full token:
  `wanctl scoped-token create --device D --command 'CMD' [--days N] [--label L]`,
  `wanctl scoped-token list`, `wanctl scoped-token revoke ID`.
  Relay endpoints: `POST /u/scoped-tokens`, `GET /u/scoped-tokens`,
  `POST /u/scoped-tokens/revoke`. They use the legacy `r.auth`, which refuses
  scoped and delegated tokens, so a scoped token cannot mint or revoke tokens.
- Only the owner's own devices (not devices shared *to* them) can be scoped.
- Expiry is mandatory: default 90 days, maximum 365. A forever-token is the
  thing this feature exists to avoid; owners rotate instead.
- At most 50 live scoped tokens per namespace.
- Revocation reuses `tokens.revoked_at` (the same `RevokeToken` the portal's
  token list already calls), so a scoped token also shows up and can be revoked
  from the portal's existing token page, where it appears with `kind: scoped`.
  The portal gets no new issue form in this version.

### Enforcement: two places, and why the relay cannot be the only one

Sessions are end-to-end TLS between the controller and the device. The relay
sees ciphertext and by design cannot read the command; putting a command filter
in the relay would mean breaking E2E. So the permission is decided in two
places that each hold part of it:

1. **Relay (server).** Resolves the token on every dial and on every
   revalidation. It admits the token only to its one pinned device
   (`Access.Allows`), stamps the session with exec-only capabilities
   (`sessionauth.Exec`, not `UseCapabilities`), and puts the allowed command
   into the `sessionauth.Open` message it sends the *agent* over the
   authenticated agent control channel. The controller cannot declare or alter
   it. Any other device, `/peers` entry, account endpoint, enrollment, or
   agent endpoint is refused the same way a delegated credential is
   (`Resolve` rejects `kind='scoped'`; `wcs_` is refused by prefix on every
   namespace-only path, including satellite relays).
2. **Agent (device).** Reads the command from the relay-stamped `Open`
   message and, before any other handling of each request, requires the
   request to be *exactly* the canonical form of "exec this command": kind
   `exec`, `oneshot`, command equal byte for byte, and every other field zero
   except `spill_after` (output handling). It is an allow-list over the whole
   message, not a deny-list of known-bad fields, so a field added to the
   protocol later is rejected until someone decides otherwise. Everything else
   — file put/get/read/edit/write, logs, status, desktop, workspace, async
   exec, elevation, a different `cwd`, a different command, a pipe or `;`
   appended — gets a reject and is never executed. A changed client cannot get
   around this because the agent, not the client, holds the rule.

Each request on a scoped session also re-asks the relay `/agent/delegation-check`
(the existing per-request revalidation), so a revocation or expiry stops the
*next* request on an already-open session, and the relay's one-second lease
check closes the session itself.

### The token does not widen the device's own policy

The token narrows; it never grants. The command still goes through the device's
normal gate (mode, rules, approval, `bypass`), and the controller still has to
be one the device has paired. A leaked token used from an unknown machine
therefore also hits the device's pairing prompt, which is a second line.
The practical consequence for a headless use: the owner either runs the device
in a mode that allows that command, or adds an allow rule for it
(`wanctl rules`), once.

We considered making the token itself pre-approve its command on the device.
Rejected for v1: it would let the relay, which is not trusted with device
authority (architecture, trust model layer 3), create execute permission on a
device, which is the reverse of what a *limited* token should do.

### Old agents

An agent that does not know about scoped tokens must never serve one, because
it would ignore the extra field and run whatever the controller asks. Two
independent fail-closed guards:

- The new agent registers with `scoped_exec=1` (control register message and
  the HTTP carrier's query). The relay refuses to dial a scoped token to an
  agent that did not, with the same shape of refusal as delegation today
  (`access.Delegated && !a.delegation`). The caller sees a plain refusal.
- The `Open` message for a scoped session carries `Capabilities = exec` with a
  grant id. Agents from before this change validate a grant session as
  `Capabilities == UseCapabilities` (`Open.ValidFor`) and silently drop
  anything else, so they would not serve the session even if the relay check
  were bypassed.

An older *relay* talking to a newer agent is not a case: the token only exists
on a relay that has migration 016 and knows the kind.

### Considered and not done in v1

- Binding the token to a controller key (so a leaked token alone is useless):
  the schema leaves room (`Access.ControllerFingerprint` already exists), but
  it needs a way to learn the holder's fingerprint before issue, which is a
  second step for every use. The device's pairing gate covers the same threat
  for now.
- Argument templates, wildcards, multiple commands per token, file scopes,
  read-only scopes: not asked for; each would be its own design.
- A portal issue form, and switching Iris onto the token: next steps, not part
  of this change.
- Relay-side command filtering: impossible under E2E, see above.

## Consequences

- Leaking a scoped token exposes one command on one device, for a bounded
  time, behind that device's own policy and pairing.
- Full tokens and the WebFetch delegation behave exactly as before. The new
  `ExecOnly` field is empty for them and every new branch is guarded on it.
- A device must run a version of wanctl that includes this change to be
  reachable with a scoped token; older devices refuse. The public docs say so.
- Rollback: scoped tokens stop working the moment a relay without the code
  starts (the kind is unknown to `Resolve`), and the table can stay in place.
