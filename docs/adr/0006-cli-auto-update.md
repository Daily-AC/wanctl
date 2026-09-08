# ADR 0006 — The agent updates itself, on by default, by re-execing in place

Status: accepted (2026-09-08)

## Context

Every fix that lives on the client side reaches a device only when a human runs
`wanctl update` on it. v0.6.0 changed how device identity is derived and v0.6.1
fixed the Android device ID; both were shipped, and both sat unapplied on
devices whose owner had no reason to look. The devices that need a fix most are
exactly the ones nobody logs into: a headless box under `wanctl service`, a
laptop somebody enrolled once. The only remedy available was to message each
owner and ask them to run a command.

An agent is a long-running process that already talks to the network, already
knows how to fetch a release manifest, and already knows how to verify its
signature — `wanctl update` is that code. Nothing was missing except the
decision to run it unattended, and the two questions that decision raises:
when is it safe, and what happens to the process afterwards.

## Decision

**On by default, with one switch.** `auto_update`, values `on` / `off`,
persisted like every other setting and overridable with `WANCTL_AUTO_UPDATE`.
Default on, because a default of off would reproduce exactly the situation this
exists to end: the feature would be enabled on the devices whose owners read
release notes, which are the devices that were already current. The setting is
re-read at every check, so switching it off reaches a running agent without a
restart.

There is no interval setting and no channel setting. A first check 60 s after
start, then every 6 h plus up to 30 min of jitter, is not a number anyone has a
reason to tune, and each knob would be a permanent support surface bought
against a hypothetical.

**Exec in place on Unix.** After the swap the agent releases its lock and calls
`syscall.Exec` on its own path. The pid does not change, so systemd's MAINPID,
launchd's tracked child, the `__supervise` parent, the pid file that `wanctl
status` and `wanctl stop` read, and the inherited log file descriptors all stay
valid without any of them being taught about updates. The alternative — exit and
let a supervisor restart us — works under two of those arrangements and orphans
the device under the others, including the plain `wanctl start` case where
nothing would restart it at all.

Windows has no exec that replaces a process image, so it splits: an agent under
the Scheduled Task's `__supervise` loop exits 0 and is restarted three seconds
later, and a detached agent starts its successor the same way `cmdStart` does
and hands over the pid file.

**An idle gate, not a maintenance window.** A check that finds an open shell
session, a running background job or a live console session retries in five
minutes rather than dropping work in progress. A relay it cannot currently
reach is deliberately *not* busy: that state lasts days on a closed laptop, and
counting it would exempt the devices most in need of an unattended update.

**No rollback.** The signed release pipeline is the gate. A bad release is
fixed by shipping the next one, which this same mechanism then delivers — faster
than any rollback path, and without a second code path that only ever runs on
the worst day. Keeping the previous binary and a "revert if the new one fails to
start" heuristic would mean deciding, from inside a process that may itself be
the broken one, what "fails" means.

**No elevation, ever.** A binary in a root-owned directory logs, once per
version it cannot install, that `sudo wanctl update` is needed. A daemon that
invokes sudo is a daemon that prompts nobody and either hangs or holds a
privilege it was not given.

**The source falls back to the relay's mirror.** When `release_base` is set and
fetching or verifying its manifest fails, the relay's `/dl` mirror is tried if
it is a different URL. The mirror serves the same signed manifest, so the trust
anchor is unchanged; what changes is that a device behind an egress that cannot
reach GitHub can still update, using the one host it demonstrably reaches. This
applies to `wanctl update` too, not only the automatic path. A manifest that
verifies and offers nothing newer ends the walk: it is an answer, and asking the
mirror the same question would only produce the same answer.

## Consequences

- A fleet converges on the current release within about six hours of a
  publication, with no message to any device owner. The corollary is that
  publishing a release now changes devices: the release process, not a per-device
  decision, is the moment of deployment.
- `buildVersion == "dev"` is never replaced, so a developer's own build is safe
  from a release landing on it mid-session.
- Android stays out. The copy inside an installed APK lives in a system-owned
  directory and the app already offers "检查更新" through the package installer;
  a Termux install can write its own binary but cannot exec it directly, so the
  restart half has nowhere to land. Neither starts a loop.
- The check costs one manifest fetch per device per six hours, and an install
  costs a second one because the download follows the decision rather than
  preceding it. Holding a verified tempfile across the busy gate would save the
  round trip and leave a half-installed update parked in a bin directory instead.
- `wanctl status` grew one line, `自动更新: 开启` / `关闭`, so the first question
  asked about a version mismatch has an answer on the device.

Reverting is a `git revert` of this PR. Nothing is migrated and no wire format
changes; an `auto-update` file left in a config directory is simply not read.
