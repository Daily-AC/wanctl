# S25: desktop helpers from Windows session 0

Candidate only, based on `8692efb` (v0.21.0), on `s25/session0-desktop`.
No installation, release manifest, approval, input marker, banner, foreign-input,
focus, or desktop wire-schema changes. No real device or production relay was
used during implementation. Windows native launch/capture/input acceptance is
still required; cross-compilation and fake-call tests do not establish it.

## Implementation

- A desktop-session agent retains the inherited `__desktop` launch and original
  layout values. Only session 0 queries the active physical console user.
- `WTSQueryUserToken`, a primary duplicate, and `CreateProcessAsUser` start the
  same executable on `winsta0\default`. A full UAC token is replaced with its
  verified limited linked token; limited tokens are never elevated. The process
  gets the user's environment (`inheritExisting=false`) and profile directory.
- Windows forbids cross-session handle inheritance. Two fresh local named pipes
  carry the existing stdin/stdout frames. Their DACL permits SYSTEM and the
  selected logon SID; remote clients and additional instances are refused.
  Both connections must belong to the created child PID before any job is sent.
  No typed text, action arguments, or environment values go in the command line.
- The existing opaque layout value also binds session-0 references to the logon
  SID. Fast switching and sign-out with session-number reuse reject old IDs
  before launching an action helper. The original layout is restored before
  passing the reference to the unchanged helper guards.
- Legacy exec `screenshot` uses the helper in session 0, then transcodes its JPEG
  to the legacy raw PNG. Other sessions/platforms retain their prior capture.
- Activity events add `desktop_session` after a helper actually starts, including
  locked/failed calls without a snapshot. Typed text remains excluded.
- Helpers exit after each call. EOF cancellation and the existing two-second
  forced-stop fallback are shared by both launch paths. Pipe startup has a
  ten-second bound and reaps a child that fails to attach.

Source check: this baseline's `wanctl service install` creates a least-privilege
interactive **scheduled task** on Windows, not an SCM service (`service.go`). An
existing LocalSystem service wrapper and a SYSTEM scheduled task both enter the
new path by virtue of session 0; installing a new service is outside this change.
A custom non-SYSTEM service account cannot use WTSQueryUserToken and gets an
explicit LocalSystem requirement.

API references: [CreateProcessAsUser](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-createprocessasuserw),
[WTSQueryUserToken](https://learn.microsoft.com/en-us/windows/win32/api/wtsapi32/nf-wtsapi32-wtsqueryusertoken),
[named pipe modes](https://learn.microsoft.com/en-us/windows/win32/ipc/named-pipe-open-modes).

## Automated evidence

Tests fake process-session lookup, active-console lookup, user-token/launcher
acquisition, and the OS helper process. They exercise real helper framing,
service/store handling and legacy image conversion. They do not claim execution
of Win32 token, named-pipe, desktop, or input APIs on macOS.

| New test | Behavior removed for assertion-failure check |
| --- | --- |
| `TestSessionZeroSelectsActiveConsoleUser` | session-0 dispatch |
| `TestSessionZeroNoSignedInConsoleUser` | session-0 dispatch |
| `TestSessionZeroLockedScreenPreservesError` | session-0 dispatch |
| `TestSessionZeroSessionChangeRejectsOldCoordinates` | session-0 dispatch; separately session/logon validation |
| `TestSessionZeroConsoleChangesBeforeLaunch` | session-0 dispatch |
| `TestSessionZeroCancellationClosesInput` | session-0 dispatch |
| `TestDesktopSessionKeepsInheritedHelper` | desktop-session bypass |
| `TestSessionZeroLegacyScreenshotUsesHelper` | legacy session-0 routing |
| `TestSessionZeroLegacyScreenshotPreservesRefusal` | legacy session-0 routing |
| `TestLegacyDesktopSessionCaptureUnchanged` | legacy desktop-session bypass |
| `TestDesktopAuditRecordsSessionWithoutTypedText` | session attribution in audit |

All 11 test functions fail by assertion under the corresponding behavior
ablation, not by compilation error. The session-change test covers different
console IDs, the same ID with a new logon, and successful reuse within one logon.
The unmodified baseline cannot compile tests for the new injectable seams;
these are reversible behavior ablations, not a claim of running new tests on
that baseline without their supporting interfaces.

Logs and the reproducible ablation driver are in `artifacts/s25/` (local,
ignored build/validation artifacts). All required checks below passed with cached dependencies (`GOPROXY=off`).
The repository's additional lark build/test/vet and Android arm64 build also
passed. No dependency fetch was attempted.

```sh
go build ./...
go vet ./...
go test ./...
GOOS=windows GOARCH=amd64 go build ./...
GOOS=windows GOARCH=amd64 go vet ./...
GOOS=linux go build ./...
go test -race ./internal/desktop
```

## Owner-run Windows acceptance

Use a supervised PC with a signed-in physical console user and the existing
SYSTEM `\WanctlAgent` task. Use a controller already authorized for that PC;
retain its ordinary approval mode. Keep Defender enabled. Do not use
`wanctl service install` to set up this run: it changes the existing task's
principal. For a separately managed LocalSystem service, repeat these scenarios
under that existing service host, stopping/starting it with its manager instead.

1. Build the exact candidate on the development machine and record the commit
   and hash. Keep the previous PC executable for rollback.

   ```sh
   git rev-parse HEAD
   GOOS=windows GOARCH=amd64 go build -o artifacts/s25/wanctl-s25.exe .
   go build -o artifacts/s25/wanctl-controller .
   shasum -a 256 artifacts/s25/wanctl-s25.exe
   ```

2. At the PC, use an elevated PowerShell window to inspect the **existing** task
   and determine the wanctl executable it runs (it may be wrapped by conhost or
   a script). Set the two paths below from that inspection and the manually
   transferred candidate. Export the task, stop it, replace only its executable,
   and restart it. No new task or service is created.

   ```powershell
   $task = Get-ScheduledTask -TaskPath '\' -TaskName 'WanctlAgent'
   $task.Principal | Format-List UserId,LogonType,RunLevel
   $task.Actions | Format-List Execute,Arguments,WorkingDirectory
   # Require SYSTEM / S-1-5-18; use the exact wanctl path in the existing action.
   $agentExe = 'C:\REPLACE\existing\wanctl.exe'
   $candidate = 'C:\REPLACE\wanctl-s25.exe'
   Export-ScheduledTask -TaskPath '\' -TaskName 'WanctlAgent' |
     Set-Content -Encoding Unicode "$agentExe.s25-task.xml"
   Copy-Item -LiteralPath $agentExe -Destination "$agentExe.s25-before" -ErrorAction Stop
   Stop-ScheduledTask -TaskPath '\' -TaskName 'WanctlAgent'
   # Stop only residual processes of this exact executable, after ending the task.
   Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -eq $agentExe } |
     ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction Stop }
   Copy-Item -LiteralPath $candidate -Destination $agentExe -Force -ErrorAction Stop
   Get-FileHash -Algorithm SHA256 -LiteralPath $agentExe
   Start-ScheduledTask -TaskPath '\' -TaskName 'WanctlAgent'
   Start-Sleep 3
   Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -eq $agentExe } |
     ForEach-Object {
       $owner = Invoke-CimMethod -InputObject $_ -MethodName GetOwner
       [pscustomobject]@{PID=$_.ProcessId; Session=$_.SessionId; User="$($owner.Domain)\$($owner.User)"; Command=$_.CommandLine}
     }
   quser
   ```

   Require the candidate hash, agent session **0**, owner **NT AUTHORITY\SYSTEM**,
   and an interactive console session distinct from RDP. Record the current
   candidate process; auto-update must not have replaced it during acceptance.

3. On the macOS/Linux controller, set the authorized device ID. The following
   shell commands use the candidate controller and preserve binary stdout.

   ```sh
   ctl="$PWD/artifacts/s25/wanctl-controller"
   target='REPLACE_WITH_DEVICE_ID'
   mkdir -p artifacts/s25/acceptance
   "$ctl" screenshot --target "$target" -o artifacts/s25/acceptance/screen.jpg > artifacts/s25/acceptance/screen.json
   shot_id=$(python3 -c 'import json; print(json.load(open("artifacts/s25/acceptance/screen.json"))["snapshot"]["id"])')
   "$ctl" act --target "$target" --screenshot-id "$shot_id" --actions '[{"type":"wait","ms":8000}]' -o artifacts/s25/acceptance/wait.jpg > artifacts/s25/acceptance/wait.json
   "$ctl" exec --target "$target" --oneshot --elevate screenshot > artifacts/s25/acceptance/legacy.png
   "$ctl" logs --target "$target" --type exec --limit 20 > artifacts/s25/acceptance/events.jsonl
   ```

   Open both images and compare visible content with the physical monitor.
   Require a real image, correct monitor origin/physical source dimensions,
   `snapshot.session` equal to the console session, and `desktop_session` on
   both new and legacy activity events. A 2560x1440 source may be returned as
   1280x720 JPEG; do not mistake the intentional helper resize for bad DPI.
   During the wait, inspect the helper in Task Manager Details (enable User
   name, Session ID, Elevated and Command line columns beforehand): the helper
   must belong to the console user/session, not SYSTEM; for a normal split-UAC
   desktop it must show **Elevated: No**. Its command line contains only the
   private pipe name. After completion no `__desktop-pipe` process remains.

4. On the PC, open one ordinary, blank Notepad window. Take a fresh screenshot,
   then generate an action file from that screenshot's Notepad PID:

   ```sh
   "$ctl" screenshot --target "$target" -o artifacts/s25/acceptance/notepad.jpg > artifacts/s25/acceptance/notepad.json
   python3 - <<'PY'
   import json
   shot = json.load(open('artifacts/s25/acceptance/notepad.json'))
   windows = [w for w in shot['snapshot']['windows'] if w['process'].lower() == 'notepad.exe']
   assert len(windows) == 1, 'Open exactly one ordinary blank Notepad window, then recapture'
   actions = [{'type':'focus','pid':windows[0]['pid']}, {'type':'type','text':'S25_ACCEPTANCE_TEXT'}]
   json.dump(actions, open('artifacts/s25/acceptance/type.json','w'))
   PY
   shot_id=$(python3 -c 'import json; print(json.load(open("artifacts/s25/acceptance/notepad.json"))["snapshot"]["id"])')
   "$ctl" act --target "$target" --screenshot-id "$shot_id" --actions-file artifacts/s25/acceptance/type.json -o artifacts/s25/acceptance/typed.jpg > artifacts/s25/acceptance/typed.json
   "$ctl" logs --target "$target" --type exec --limit 20 > artifacts/s25/acceptance/type-events.jsonl
   ```

   Require the text in Notepad, normal approval behavior, visible existing act
   banner, and only action type/character count in the log, never the literal
   `S25_ACCEPTANCE_TEXT`. Close this test document without saving afterward.

5. Verify environment and effective rights with an existing `launch` action.
   Save this as `C:\Users\Public\s25-context.ps1` on the PC and first run it from
   an **ordinary console-user PowerShell** to record a comparison. Then delete
   its output and run it through `act launch` with a fresh screenshot ID.

   ```powershell
   $id = [Security.Principal.WindowsIdentity]::GetCurrent()
   $principal = [Security.Principal.WindowsPrincipal]::new($id)
   [pscustomobject]@{
     User=$id.Name; Session=(Get-Process -Id $PID).SessionId
     Admin=$principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
     Profile=$env:USERPROFILE; Temp=$env:TEMP
   } | ConvertTo-Json | Set-Content "$env:USERPROFILE\s25-context.json"
   Start-Sleep 15
   ```

   The action array is:

   ```json
   [{"type":"launch","program":"powershell.exe","args":["-NoProfile","-File","C:\\Users\\Public\\s25-context.ps1"],"timeout_ms":10000}]
   ```

   Require the same User, Session, Admin, Profile and Temp as the ordinary-user
   run.
   If local execution policy blocks this test script, use the Task Manager
   helper-token observation above and mark this environment probe unverified;
   do not weaken execution policy. Remove the script and JSON after inspection.

6. For each case below take a **fresh** screenshot first; IDs are single-use and
   expire after two minutes. Keep switching/sign-out tests inside that TTL so
   expiry does not masquerade as session validation.

   - Lock with Win+L. Both screenshot commands and an act call must return the
     existing `screen locked or secure desktop active` error, with no image or
     input. Repeat while UAC's secure desktop is showing; dismiss it locally.
   - Switch to another already-created console user. `act` with the old ID must
     return `stale screenshot_id` without a helper or input in that new session.
     Take a new screenshot and confirm its new session. Repeat by signing out
     and signing in; a reused numeric session must still reject the old ID.
   - Sign out every console user. Both screenshot commands must explain that no
     user is signed in at the active console. Repeat with only an RDP user
     signed in; the agent must not select that remote desktop.
   - With an unlocked console, run an 8-second wait followed by a test typing
     action; move the physical mouse during the wait. Require the existing
     foreign-input interruption and no typed suffix. Repeat using the banner's
     stop button, and with controller Ctrl+C during a wait. Require helper exit
     and no held keys/buttons. Do not automatically replay interrupted input.
   - Repeat ordinary capture/action while the agent runs in an existing desktop
     session, using that setup's existing launch method; behavior stays as in
     v0.21.0. Do not register another task for this check.

7. Record Defender state/detections for the exact candidate and run interval,
   plus helper PID/session/owner observations, screenshots, JSON results and
   activity events. A previous PowerShell experiment is not Defender evidence
   for this executable. Confirm no helper remains after calls or cancellation.
   Restore the previous executable using step 2's stop/replace/start sequence
   with `$candidate = "$agentExe.s25-before"`; retain the original task definition
   and config. Remove only acceptance artifacts after retaining the evidence.
