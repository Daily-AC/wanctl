# 0020 — Android act relays to the OpenCU Android engine

Date: 2026-11-01
Status: accepted; implementation in progress

## Decision

`wanctl act` / `wanctl_act` OpenCU mode ([ADR 0017](0017-act-relays-to-opencu.md))
works on Android devices running the wanctl APK. The device agent starts the
OpenCU Android engine (OpenCU ADR 0005: `opencu-android.jar`, a Java
`app_process` that runs as the `shell` user and speaks MCP on stdin/stdout)
through the existing elevation channel and relays MCP exactly as on Windows:
`discover` and `{tool, arguments}`, no tool names in wanctl, same session key
(authenticated controller fingerprint + this device), same busy lock, idle
close, stop latch, approval and audit. Legacy `actions[]` stays Windows-only.

No new APK screen, setting or permission. The elevation channel's existing
switch is the owner's opt-in; act's existing approval policy applies to every
call. This stays within the APK's frozen role of controlled-device agent.

## Starting the engine

- **Channel.** su when available, otherwise the phone's own wireless-debugging
  adbd, in the channel's usual order; `doctor`-style failures map to the
  channel's existing link states so the person sees one actionable sentence
  (turn wireless debugging back on, pair again, enable the elevation channel).
  The engine needs a bidirectional stream: adb uses the `shell,v2` protocol with
  stdin packets and close-stdin, on its own adb connection so ordinary elevated
  commands are not blocked while a session is open.
- **Placement.** The jar is copied as shell to
  `/data/local/tmp/opencu-android-<sha256-16>.jar` (create-then-rename,
  mode 0644) and its SHA-256 is checked on the device before every start. It
  runs as `CLASSPATH=<jar> app_process / dev.opencu.android.Main mcp`.
- **Lifetime.** One engine process per OpenCU session; stdin EOF ends it. The
  engine's lifetime is the channel's: when wireless debugging drops, the call
  in flight returns an unknown outcome and nothing is replayed.

## Getting the engine

The agent fetches the engine from the official OpenCU GitHub release on first
use and caches it in the app's files directory by hash. Trust root: HTTPS to
github.com and the SHA-256 in the same release's `opencu-android.json`
manifest (version, file name, size, SHA-256), the same level as the Windows
`install.ps1`, which embeds the artifact hash ([ADR 0019](0019-portal-triggered-installs.md)).
The cache is refreshed at most once a day and never while a session is open; a
failed refresh keeps using the cached engine. The agent reports the cached
OpenCU version at registration, so the portal's existing version display works
for Android without portal changes. `WANCTL_OPENCU_ANDROID_URL` (a directory
holding the manifest and jar) overrides the source for testing and self-hosting.

## Consequences

- Android act depends on the elevation channel: wireless debugging (cleared at
  reboot, on Wi-Fi changes), or root.
- The shell user can do far more than the engine's tools. The engine opens no
  port and reads only its own stdin; wanctl's approval, audit and stop rules are
  the boundary, as for elevated exec.
- A compromised GitHub release could ship a malicious engine; the same is true
  of the Windows installer today. Signing OpenCU releases is out of scope here.
