# ADR 0007 — A shared device gives its grantee the owner's rights

Status: accepted (2026-09-09)

## Context

A controlled device runs one agent, bound to one account. That is not an
implementation detail waiting to be lifted; it is what the device's trust model
is built on. The agent holds one identity, one set of trusted controller
fingerprints, one rule file, one mode. There is no per-user view of any of it.

Sharing was designed as if that were not true. A grant carried a permission
subset — `exec`, `read`, `write`, in any combination — that the relay parsed
and stamped onto the grantee's session, and `console` and `logs` could not
appear in a grant at all. The result was a second permission model, sitting
above the device's own, disagreeing with it, and weaker than it in ways nobody
could act on: a grantee holding `exec` on a device left in bypass mode could
run anything on it, while the same grantee was refused `wanctl logs` on the
device whose shell they already had. The subset was not protecting the owner
from the grantee. It was deciding which of two doors into the same room was
locked.

Sharing is how a machine that only one account can own gets more than one user.
A lab box, a build machine, a family computer: the owner enrolls it, and
everyone else reaches it through a grant. Treating those people as second-class
controllers of a machine they are expected to work on produced exactly the
support question this ADR ends — "why can my colleague run commands but not
read the log of what they ran".

## Decision

**A grant is owner-equivalent.** The relay stamps a grantee's session with
`sessionauth.FullCapabilities`, the same set an owner's session carries:
exec, read, write, logs, console. `sessionauth.ParseGrant` and the
`GrantCapabilities` ceiling are deleted rather than defaulted, so there is no
code path left that can quietly reintroduce a reduced grant.

**The device is the permission model.** What a grantee may actually do is
decided where it was always decided: by the device's mode, its rule set, and
its approver. One mode and one rule set apply to everyone. An owner who leaves
a device on per-request approval answers for the grantee's commands, one at a
time. An owner who puts it in bypass has bypassed it for the grantee too, and
that is the honest reading of what bypass already meant. Elevated exec stays
outside bypass for everyone, unchanged.

**The control plane stays with the owner, on the device's terms.** `console`
is now in the grantee's session, but the agent independently requires the
connecting fingerprint to be in its `portal_admins` set before it will serve
approvals, rules, mode or trust. That check is the one that keeps "the owner
approves" true, and it is on the device, where it belongs, instead of being
inferred from a string in the relay's database. Unbinding a device and revoking
a share stay owner-only.

**`acl.perms` is kept and stopped.** New grants write the constant `full`;
nothing reads the column. Keeping it costs a column and no migration, and it
means a relay rolled back to the previous binary finds values it can still
parse. Dropping it would buy tidiness at the price of a schema change on a
table that carries live authorization state. The `perms` field is likewise
still accepted on `/admin/acl` and `/u/shares/grant` and ignored, so an older
CLI and the portal's existing share form keep working; `wanctl share grant`
loses its `--perms` flag, because a flag that silently does nothing is worse
than one that is gone.

## Consequences

- Every existing grant widens the moment this deploys. A share that was written
  as `read` becomes owner-equivalent on the grantee's next dial. There is no
  migration to run and no way to keep the old meaning, which is the point: the
  old meaning was not enforceable at the device anyway. Owners who shared a
  device on the strength of the narrower wording should revoke rather than
  re-scope — revoking is still theirs alone.
- `wanctl logs` and file writes now work for a grantee, subject to the device's
  policy. `wanctl share list` no longer prints a permissions column, because
  every share has the same one.
- The audit finding that produced the portal's read-only rule for shared
  devices (2026-08-28, SEC-B-01) described a *read-only* share reading the
  owner's control plane through the portal's privileged token. That premise is
  retired by this decision — there is no read-only share. The gate in
  `requireOwnedConsole` is nevertheless left standing here, and the portal half
  of this change is a separate piece of work: relaxing it needs a UI that shows
  a grantee whose device they are administering, and a decision about the one
  genuinely personal thing on that page, the owner's notification address,
  which is the owner's contact detail rather than device state.
- The relay stops being a place where sharing can be tuned. That is a
  deliberate loss of a feature: an owner who wants a colleague to read files
  but not run commands can no longer express it here, and must not share the
  device. Expressing it properly would mean per-controller rules on the device,
  which is where the enforcement already lives — a larger change than this one,
  and not one anyone has asked for.

Reverting is a `git revert` of this PR. Grants written while it was deployed
carry `full` in `acl.perms`, which the previous binary parses as an unknown
capability and fails closed on, so a revert must be followed by rewriting those
rows to a valid subset.
