# Desktop act: supervised candidate acceptance

This kit is **not an automated device test**. Do not run it without the device
owner and someone at the screen. The candidate is not a release. Do not change
production manifests, services, scheduled tasks, installed binaries or rules.
No screenshots of a household desktop belong in this repository or in a report.
Use a separate, disposable test config and relay; keep all images/observations local.

## Prepare and start an isolated session

1. Arrange an idle period on the living-room PC. Record the candidate SHA256,
   Windows build, monitor arrangement, resolution, DPI, agent user/session and
   Defender engine/signature versions. Retain the known-good installed binary
   and its usual start method. Windows 10 2004+ and a logged-in **console** user
   are required; SYSTEM/session 0 and RDP-only sessions are outside this version.
2. Copy `wanctl-s23.exe`, `index.html` and `SHA256SUMS` from the candidate bundle
   to a local test directory. Verify with `Get-FileHash .\wanctl-s23.exe`.
   Keep real-time protection enabled; record `Get-MpComputerStatus` and
   `Get-MpThreatDetection` before and after. Stop acceptance on a detection;
   do not add exclusions. A new build needs its own check.
3. Use a disposable token, generated locally (do not use a production token).
   On a trusted test LAN, start the candidate relay on the controller computer:

   ```sh
   export WANCTL_CONFIG_DIR="$PWD/s23-controller-config"
   export WANCTL_RELAY="ws://CONTROLLER_LAN_IP:18080"
   export WANCTL_TOKEN="LOCAL_DISPOSABLE_TOKEN"
   export WANCTL_TOKENS="$WANCTL_TOKEN:s23"
   unset DATABASE_URL WANCTL_UPSTREAM_RELAY WANCTL_MCP_SEED WANCTL_PUBLIC_ORIGIN
   ./wanctl-s23 relay --addr CONTROLLER_LAN_IP:18080
   ```

   Bind to the test LAN address, not a public interface. Start a second terminal
   with the same controller environment for commands. In an ordinary PowerShell
   window on the Windows desktop, start the candidate with a **different** config:

   ```powershell
   $env:WANCTL_CONFIG_DIR = "$PWD\s23-device-config"
   $env:WANCTL_RELAY = 'ws://CONTROLLER_LAN_IP:18080'
   $env:WANCTL_TOKEN = 'LOCAL_DISPOSABLE_TOKEN'
   $env:WANCTL_TRANSPORT = 'ws'
   .\wanctl-s23.exe config set auto_update=off
   .\wanctl-s23.exe agent --name s23-desktop-test --yes --mode bypass
   ```

   These pairing/bypass settings apply only to the disposable test agent. The
   candidate keeps build version `dev`, so it does not seek automatic releases.
   It installs nothing and does not replace the normal agent. Record the device
   fingerprint with `wanctl-s23.exe id` in another terminal using that same
   **test device** config. On the controller, set a descriptive label and pin
   the fingerprint independently verified at the Windows screen:

   ```sh
   ./wanctl-s23 label "Owner's supervised S23 agent"
   ./wanctl-s23 trust server --target s23/s23-desktop-test --fingerprint SHA256:VERIFIED
   ./wanctl-s23 screenshot s23/s23-desktop-test -o before.jpg > before.json
   ```

4. Open `index.html` on Windows in Edge/Chrome at 100% browser zoom, press F11,
   then leave its keyboard and mouse alone. On multiple monitors, put one copy
   on each display. Verify all four markers in the JPEG and compare metadata
   with Windows Display settings. Repeat at 100%, 125%, 150%, with a monitor
   left/above the primary (negative origin), and with mixed-DPI monitors.

## Click precision and basic actions

- Set the page's scale X to `snapshot.width / snapshot.source.width`, and Y to
  `snapshot.height / snapshot.source.height`. Finish this manual setup **before**
  taking the reference screenshot. Keep both original JPEG bytes and JSON.
- Choose the center (+) of a numbered target in that JPEG. Put this in
  `actions.json`, replacing coordinates with what was actually observed:

  ```json
  [{"type":"click","x":64,"y":72,"button":"left","count":1}]
  ```

  ```sh
  ./wanctl-s23 act s23/s23-desktop-test --screenshot-id ID_FROM_BEFORE_JSON \
    --actions-file actions.json -o after.jpg > after.json
  ```

- The **page's recorded target number**, not the tool's input_sent result, is the
  external pass/fail oracle. Export its observations locally. Report every
  intended/actual target, requested screenshot coordinate, error vector and
  distance in **screenshot pixels and physical pixels**. The page computes
  physical error as `(client point - target center) × devicePixelRatio`; screenshot
  error uses the two image scale factors above. Coordinates must not be resized
  by the viewer. Allow integer selection/rounding error, not a systematic DPI
  offset; report maximum and median error, not just a pass label.
- Also click a target under the banner's initial location, and drag through that
  area. The banner must move away, stay visible, and let the underlying target
  receive the input; it must not turn an excluded area into an unclickable strip.
- Use the returned fresh ID for each new act. Check all targets, left/right and
  double click, drag, scroll, Unicode (including emoji), ctrl+l, alt+f4, enter,
  wait, focus by unique title/PID, and launch. Launch a harmless program such as
  `notepad.exe`; record PID, window_appeared and foreground separately, and
  confirm it remains alive after the helper exits. A reused-process launcher
  needs an unambiguous expected `title`.
- Crop with `screenshot --screenshot-id FULL_ID --region X,Y,W,H`. Verify the crop
  has a new ID, its source is physical, and a click uses **crop-local** pixels.
- Connect an MCP host to this candidate's `mcp` stdio command, with the same test
  controller environment/config. Call `wanctl_screenshot` then `wanctl_act` with
  `target`, `screenshot_id` and the same action objects. Verify raw JPEG bytes,
  dimensions and ID survive unchanged. Test a legacy agent and a non-Windows
  agent only on the isolated lab relay; the PNG fallback has no act ID.

## Failure matrix (S21 §5.8, S-A item 2)

| Case | Setup and required observation |
|---|---|
| Wrong, stale or another controller's ID | Invent an ID, wait at least two minutes, or change controller identity. Zero input; explicit refusal. Restart the test agent and verify previous IDs are unknown. |
| DPI/display/session change | Capture, change scaling/rotation/layout or disconnect a monitor, then act with the old ID. Refuse before input. Also change a display during a wait; queued actions must not run. |
| Covered target | Capture a target, cover it with another window before act. No click through the covering window. |
| Foreground stolen | Switch windows after the snapshot, and arrange a timed popup during a long synthetic type. Stop before typing into the new window. Explicit focus must select exactly one captured window. |
| Locked/no user/secure desktop | Lock, sign out/switch user, or open UAC. Clear unavailable error, no image/input; unlock manually. Do not infer that an unavailable desktop specifically means UAC. |
| Elevated window | Put an administrator window in front. Metadata identifies elevation; type/key/click refuse. Do not elevate the helper. |
| Human input before first action | Hold a key or mouse button as act starts. Zero synthetic input; return `有人在用这台电脑`. |
| Human input mid-type | Use a long synthetic string followed by a click; move the mouse or press a key during typing. Stop input within **200 ms**, cancel the click, release held input, return the exact human-input result. |
| Human input mid-wait/drag | Queue a 10 s wait or drag, then a click. Move the mouse/press a key mid-action; same 200 ms limit, no queued click, no stuck button/modifier. |
| Stop button | Repeat during wait/type using the visible Stop button. The banner must name the controller, remain visible over a fullscreen app, and never take foreground focus or appear in the JPEG. |
| Between calls | A person uses the PC while no call is active. No resident helper/hooks remain. A later explicitly authorized call starts a fresh input monitor; there is no lease or cooldown. |
| Controller disconnect | Disconnect the test controller during wait/type/drag. Cancel queued input and release held state. Controller reports partial/unknown; no automatic reconnect/replay of act. |
| Helper crash | Terminate only the candidate's `__desktop` child from the controller during a harmless batch. Agent reports state unknown and remains responsive. Inspect the desktop before any new action; never resend the batch. |
| Duplicate delivery | Submit the identical act again with its consumed screenshot ID, including after a failed call. No additional page event; report state unknown. After eviction/restart, the old ID remains unusable. |
| Partial batch | Run wait, an intentionally ambiguous focus, then type. Completed count is 1, failed_index is 1, final type never executes. Inspect any new screenshot before further work. |
| Privacy | Use `S23_SYNTHETIC_PRIVATE_中文_🚀` in type. Inspect normal-mode approval, local activity log and captured test notification payloads: no text, only action kinds/coordinates/character count. Images/window titles may contain it and stay local. Verify bypass still auto-allows ordinary exec/act. |

Measure stop latency independently (e.g. a high-frame-rate recording of human
input and the page's last synthetic effect), not RPC duration. Record each
trial and measurement resolution. Check normal typing/clicking afterward for
stuck modifiers/buttons. A hard OS process termination can make held-state
recovery uncertain; do not claim the crash case is safe from a fake test alone.

## Acceptance gate and cleanup

After the matrix, run the original living-room task three times with a new
agent: game closed, the existing short-video app in front; reach the game lobby
using only wanctl tools, no handwritten input/PInvoke scripts or game changes.
A person at the TV confirms the actual display. Each run ≤3 minutes, median
≤2 minutes, ≤8 tool calls; record calls, atomic actions and interventions. Add a
human mouse interruption trial: the calling agent must ask its user and must
not retry automatically. Any failed row returns the candidate for repair.

Stop the test agent and relay with Ctrl-C; close programs opened by the test;
remove the disposable configs/token after retaining private evidence. The
normal installation was never overwritten: resume its existing start method
if it was paused. Before any later installation/update-path test, separately
prepare the known-good installer and owner-readable rollback steps. No release
or Defender claim follows from the macOS tests or cross-compilation alone.
