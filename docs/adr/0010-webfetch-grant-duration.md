# 0010 — WebFetch grant duration: a grant long enough to walk away from

Date: 2026-09-18
Status: accepted

## Context

ADR 0009 raised the per-operation ceiling so a Blender render could finish, and
closed by naming what it had not fixed:

> A thirty-minute exec works, but only inside a grant that is long enough: the
> deadline is still clamped to `access.ExpiresAt`, and a grant is at most 60
> minutes.

That is the ceiling the owner actually hits. The work a web AI is asked to start
on a device is frequently not interactive: a build, an install, a render, a
model download. The human kicks it off, closes the tab, and comes back to the
same chat later to read `result_url`. With a 60-minute maximum that shape is
impossible to express. The owner either approves an hour and watches the job get
clamped to the end of the grant, or approves an hour and comes back to a grant
that has expired with the result unreadable — the job ran, and its outcome is
now behind a credential that no longer resolves.

`exec`'s own 1800-second ceiling has the same problem from the other side. Half
an hour covers a compile; it does not cover a long render, a large model
download or a full OS package upgrade, which is the class of work the tool is
being used for.

The 64-job ledger cap was sized against the same hour. Kept flat, a grant that
lasts a day would spend its whole allowance in the first hour and then refuse
every further operation while still being perfectly valid.

## Decision

**Grants may be approved for 1 to 1440 minutes.** `delegation.MaxGrantMinutes`
is the one place that number lives. The approval page offers 15 minutes, 1 hour,
4 hours and 24 hours as presets plus a free-form minutes field, and the default
is still 15 minutes: the common case is still a short piece of work, and a
longer grant is something the owner chooses deliberately rather than something
the page nudges them into. The presets write into the free-form field, so
whichever control the owner touches, the server receives one `minutes` value and
validates it identically.

**The browser ticket envelope is derived, not written down.** It was 70 minutes:
the old 60-minute maximum plus the 10 minutes a ticket has to reach approval. It
is now `delegation.TicketLifetime` = `RequestWindow + MaxGrantMinutes`, 1450
minutes. This is a correctness relationship, not a tuning knob. The ticket is
how the session URL resolves; an envelope shorter than the grant would expire
the URL while the grant it names is still approved, which is exactly the case
this ADR exists to support.

**Delegation cleanup retention rises to 25 hours.** Retention is measured from
the request row's `created_at`, a row may be created a full `RequestWindow`
after its ticket was issued, and the grant approved on it may then run
`MaxGrantMinutes`. So a live ticket can name a row up to 1440 minutes after that
row was created, and the old 24-hour floor became exactly the boundary rather
than safely past it. `delegation.MinRetention` is the maximum grant plus an
hour, and the store refuses anything shorter.

**`exec` gets 1–14400 seconds, default 300.** Four hours is what a grant long
enough to be walked away from can usefully spend on one operation.
`DefaultExecSeconds` does not move: ADR 0009 established that an omitted
`timeout_seconds` stays omitted from the canonical payload precisely so the
default can change without changing a call's replay identity, and there is no
reason to spend that here. File tools stay at 1–60 seconds; nothing about their
argument changed. A job's deadline is still the earlier of the requested timeout
and the end of the grant.

**The job ledger allowance scales with the approved duration.**
`delegation.MaxJobs` is the single function: 64 jobs per approved hour, partial
hours rounded up, never fewer than 64. A 15-minute and a 60-minute grant both
allow 64, a 61-minute grant allows 128, a 24-hour grant allows 1536. The rate is
the old number, so nothing that worked before behaves differently; only the
grants that did not exist before get more. The allowance is computed from the
window the owner approved — `decided_at` to the token's expiry, both written by
the approving transaction — and not from the time remaining, so it does not
shrink under a long-running client.

Concurrency is unchanged: 4 operations in flight per grant, 64 across the
adapter. Those bound instantaneous load, which a longer grant does not increase.

## Consequences

- **A status URL is a bearer credential for up to 24 hours.** That is a real
  change in exposure, so the approval page says it in the fine print rather than
  only in the docs: the longer you approve, the longer the AI's session link
  keeps working, and anyone holding the link can act as that client until then.
  The remedy is the one that already exists — revoke from **Settings → Access
  tokens**, which invalidates the grant immediately, ahead of its expiry.
- **The rid replay window from ADR 0009 now lasts up to 24 hours.** That ADR
  accepted a one-time 409 for pre-deploy jobs replayed with no `timeout_seconds`
  on the grounds that "grants live at most 60 minutes, so the window closes by
  itself." The window is now up to a day. This is accepted rather than fixed: a
  409 was already the safe answer, the affected callers are those replaying a
  URL across a deploy, and a compatibility shim for a 24-hour window is more
  machinery than the failure justifies. The reasoning has been corrected in
  place in ADR 0009 so nobody re-derives the old bound from it.
- A long grant still cannot outlive its devices. Device removal or certificate
  rotation invalidates it on the next resolve, exactly as before, and the
  resolve happens per operation rather than once at approval.
- Nothing about what a grant may do changed. It is device use only, device
  policy still decides each operation, and pairing is still the human's.
