## Portal-triggered device installs

The authenticated portal console accepts `package_install` with only
`package` and `version`: `wanctl` + an exact `vX.Y.Z` / `vX.Y.Z-beta.N`, or
`opencu` + `latest` (Windows). It acknowledges with `package_install`, or
returns `error` with a plain `reason`. Unknown kinds on older agents already
return `error` with Data `"unknown RPC kind"`; the portal turns this into the
hint: "This device's wanctl is too old to be upgraded from the web. Run
`wanctl update` on the device; if auto-update is on, it will also update itself."

`install_progress` is an unsolicited device-to-portal message, never an RPC
reply. Its Data is `{package, version, stage, reason?}`. Stages are
`downloading`, `installing`, `done` (OpenCU only) and `failed`. OpenCU success
carries the newly read installed version. wanctl stays installing until the
portal observes the target `wanctl_version` in an online registration. Progress
is kept per owner/device in portal memory and expires after ten minutes with
a device-log hint. It is not restored after a portal restart.

`GET /api/devices/install?device=ID` returns `{}` or
`{package, version, stage, reason?, started}`. `POST` to the same path accepts
only `{package, version}` and returns that state. Both require the device's
owner; shared managers and administrators of other owners' devices are
refused. POST uses the same origin and CSRF double-submit checks as other
portal mutations. Offline devices cannot start an install. The relay carries
the console as opaque E2E bytes and needs no console-kind allow-list change.
Portal and relay are deployed together.

The device validates wanctl tags with `internal/release`, refuses same-version
installs and downgrades, and uses the CLI updater's signed manifest, exact
artifact and SHA-256 checks. Beta requests additionally try
`https://github.com/Daily-AC/wanctl/releases/download/T/`; stable configured/mirror source order
is unchanged, with that exact tag as a final fallback for intermediate stables.
Development builds, APK copies, standalone Android/Termux and unwritable binary
directories are refused before downloading, as with automatic updates. Web
upgrades use the same restart path and assumptions: Unix exec preserves the
PID; Windows `--managed` exits so the external supervisor restarts the same
path, including SYSTEM PowerShell loops and older `__supervise` parents.
Unmanaged Windows agents start a successor, including in session 0. No parent
marker or additional supervisor trust gate is required. Explicit owner
requests do **not** depend on `auto_update`; its schedule and own decision
table remain unchanged.

If all sources report the tag absent (404 manifest or a verified different
tag), progress says `version T was not found in the official releases`.
If a source cannot be reached, progress says `the device could not reach HOST`
with only its hostname, even if another source returns 404. Other installation
failures direct the owner to device logs. Full per-source diagnostics are
recorded in the device event log, not sent to the portal. The UI localizes the
plain sentences and uses the full-width colon in Chinese.

OpenCU downloads only the official fixed latest-release installer at
`https://github.com/Daily-AC/opencu/releases/latest/download/install.ps1` and
runs it in the signed-in console user's session and LOCALAPPDATA. SYSTEM
launches the existing helper with the user's token and environment. A missing
console user or an elevated inherited user token is refused. Idle wanctl
OpenCU connections are closed under the desktop lock; active calls are
refused. The five-minute helper timeout contains its process tree; output is
bounded to 16 KiB and errors to the cleaned final 400 characters. Trust is
HTTPS to github.com plus the zip SHA-256 inside the official script, as in the
manual install; no wanctl signature is claimed for OpenCU.

Every request and outcome is an `install` event in `wanctl logs`. A concurrent
install or an agent busy with a session, job, desktop call or approval is
refused rather than queued. On OpenCU success, WebSocket agents send
`{op:"versions", wanctl_version, opencu_version}` on their existing control
connection; only the currently registered connection may update metadata.
HTTP agents use their next existing `/h/poll`. Neither needs to reconnect;
the console progress gives the portal the OpenCU version immediately.

After a successful binary swap, the agent retains its installed version and
restart-pending state under the shared installation lock. Web and automatic
updates cannot start another install until the process is replaced. A CLI
swap of the executable is detected by its changed file identity. Failed
installs release the reservation without marking restart pending.

Device progress is untrusted: the portal applies `versionmeta.Clean` to its
version, removes control/format characters from its reason and retains at
most 400 characters before storing it. The relay persists version metadata
at most once per device every ten seconds after an attempt, coalescing to the
latest report and flushing pending changes when the interval ends. Identical
reports remain deduplicated; database failures use the same minimum interval.
