# ADR 0007 — A share inherits the owner's use; management is one switch

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

**Use is inherited.** The relay stamps a grantee's session with
`sessionauth.UseCapabilities` — exec, read, write, logs — which is the owner's
own session minus its control plane. `sessionauth.ParseGrant` and the
`GrantCapabilities` ceiling are deleted rather than defaulted, so there is no
code path left that can quietly reintroduce a permission subset.

**Management is one switch, off by default.** `acl.manage`, added by migration
008, adds `Console` to that grantee's sessions and lets the portal open the
device's console for them: approvals, rules, mode, trusted controllers. The
owner sets it when sharing (`wanctl share grant --manage`) or afterwards
(`wanctl share manage --device D --to NS on|off`), which flips it without
revoking the share and making the grantee pair with the device again.

It is one bit rather than a matrix on purpose. A matrix is what this ADR is
undoing: the previous design had four expressible subsets, and every one of
them was decided again, differently, by the device's mode and rules. The only
distinction the device itself draws is between using it and administering it —
that is exactly the line `portal_admins` and the console hello already draw —
so that is the only distinction a share can carry. Anything finer belongs in
the device's rule set, where it is enforced, and would be the same feature for
owners and grantees alike.

**The device is the permission model.** What a grantee may actually do is
decided where it was always decided: by the device's mode, its rule set, and
its approver. One mode and one rule set apply to everyone. An owner who leaves
a device on per-request approval answers for the grantee's commands, one at a
time. An owner who puts it in bypass has bypassed it for the grantee too, and
that is the honest reading of what bypass already meant. Elevated exec stays
outside bypass for everyone, unchanged.

**Two independent checks guard the control plane.** Even with the switch on,
the agent requires the connecting fingerprint to be in its `portal_admins` set
before serving approvals, rules, mode or trust, so a grantee's own controller
still cannot administer the device directly; the portal, which is a console
administrator, is how a managing grantee reaches it. The relay switch says
whether the owner agreed; the device's own set says who may act. Neither is
inferred from a string in a database.

Unbinding a device, revoking a share, renaming it, and the owner's notification
settings are never granted. Notifications are the owner's contact details
rather than device state, so they stay behind an owner-only gate even for a
managing grantee.

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
  as `read` becomes full use of the device on the grantee's next dial. There is
  no way to keep the old meaning, which is the point: the old meaning was not
  enforceable at the device anyway. Owners who shared a device on the strength
  of the narrower wording should revoke rather than re-scope — revoking is
  still theirs alone. No existing share gains management: migration 008
  defaults the column to false, so the control plane stays exactly where it was
  until an owner says otherwise.
- `wanctl logs` and file writes now work for a grantee, subject to the device's
  policy. `wanctl share list` no longer prints a permissions column, because
  every share has the same one.
- The audit finding that produced the portal's read-only rule for shared
  devices (2026-08-28, SEC-B-01) described a *read-only* share reading the
  owner's control plane through the portal's privileged token. There is no
  read-only share any more, so the gate becomes "did the owner turn management
  on" instead of "is this the owner": `requireOwnedConsole` splits into
  `requireDeviceConsole`, which the switch opens, and `requireDeviceOwner`,
  which nothing does. The exposure the finding described is now something an
  owner chooses per share rather than something the code prevents.
- The portal's device activity log stays behind the management gate, while the
  same log is available to any grantee through `wanctl logs`. That is
  inconsistent, and it is inconsistent in the safe direction; which way to
  resolve it belongs with the portal UI work.
- The relay stops being a place where sharing can be tuned, apart from the one
  switch. An owner who wants a colleague to read files but not run commands can
  no longer express it, and must not share the device. Expressing it properly
  would mean per-controller rules on the device, which is where the enforcement
  already lives — a larger change than this one, and not one anyone has asked
  for.

Reverting is a `git revert` of this PR. `acl.manage` is left in place by a
revert and simply stops being read; migration 008 uses `ADD COLUMN IF NOT
EXISTS`, so re-applying it later is a no-op. Grants written while this was
deployed carry `full` in `acl.perms`, which the previous binary parses as an
unknown capability and fails closed on, so a revert must be followed by
rewriting those rows to a valid subset.
