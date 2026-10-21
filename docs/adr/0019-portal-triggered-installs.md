# 0019 — Owner-triggered device installs

Date: 2026-10-21
Status: accepted

## Decision

The versions section of a device's portal detail page lets its owner request
`wanctl` at an exact, strictly newer stable or beta tag, or `opencu` at `latest`
on Windows. Shares, including management shares, and instance administrators
on someone else's device do not carry this right. The HTTP route enforces
ownership and the portal's normal CSRF check.

These are the only two packages: wanctl already has a signed self-updater;
OpenCU is the per-user engine wanctl uses for computer-use calls. This is not
a general remote package manager. The console request contains only package
and version, never a URL, script, command, install directory or schedule.

wanctl uses the same Ed25519-signed manifest, exact-artifact selection and
SHA-256 verification as `wanctl update <version>`. Exact betas additionally
try their official GitHub tag directory, even with no `release_base` or a
stable-only relay mirror. Stable configured/mirror source order is unchanged, with the exact GitHub tag
as a final fallback so an intermediate stable can also be selected. Sources choose
where bytes come from; signatures determine whether to trust them. No
same-version install, downgrade or rollback is offered by the portal.

OpenCU runs the official fixed latest-release `install.ps1` in the signed-in
console user's session, with that user's environment. SYSTEM uses the existing
user-token helper. Its trust root is HTTPS to github.com and the artifact
SHA-256 embedded in that official script, exactly as in the documented manual
install. It is not a wanctl-signed release. The script retains its own
concurrent-install and running-process guards.

## Refusals and completion

Only one install runs on a device. Active sessions, jobs, desktop calls or
pending approvals are refused, not queued. The device logs requests and
outcomes. Explicit owner requests ignore `auto_update`; automatic scheduling
and version decisions remain unchanged.

wanctl uses the automatic updater's eligibility and handover assumptions:
development builds, APK copies, standalone Android/Termux and unwritable
binary directories are refused before downloading. Unix re-execs, preserving
the PID for foreground, detached, launchd, systemd and supervisor runs.
Windows `--managed` means an external supervisor restarts the same binary path
after exit, including PowerShell loops running as SYSTEM and existing
`__supervise` parents. No parent marker or second supervisor trust test is
required. Unmanaged Windows agents use the automatic updater's successor
process path, including session 0; web upgrades add no stricter mode gate.
Any defect in those restart assumptions is an auto-update issue too.

If every source reports the requested tag absent, the portal names the missing
version. If a source cannot be reached, it names only the host; a later 404
must not hide that uncertainty. Full per-source errors remain in the device
event log. Other installation failures give a plain sentence directing the
owner to that log, rather than exposing raw Go diagnostics.

OpenCU refuses non-Windows devices, a missing console user, unavailable user
environment, an elevated inherited user token, and any active desktop call.
wanctl closes its own idle OpenCU peers before installing. The helper bounds
the installer to five minutes and its output to a 16 KiB tail, returning a
cleaned, 400-character final error line. A Windows job object contains its
process tree. Success re-reads the manifest; WebSocket version metadata is
sent on the existing registration connection, HTTP metadata on the next
existing poll, and the portal receives the version in unsolicited progress.

Progress lives in portal memory, not the database. wanctl is only done after
an online device registration reports the requested version; a successful
binary swap alone is still installing. After ten minutes without completion,
the page points to device logs. A portal restart loses this transient state,
not the installation or device version metadata.

Portal and relay deploy together; old portal/new agent and new portal/old
relay are not supported deployment shapes. Old agents' existing `unknown RPC
kind` reply becomes a readable suggestion to wait for their six-hour automatic
update or run `wanctl update` locally. No relay/portal release fetches are added.
