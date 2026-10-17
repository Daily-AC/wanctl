# Isolated Windows 11 acceptance: act → OpenCU

Status: reviewer procedure, not a claim of Windows execution. Use the commit
on `feat/act-opencu`. Automated Go peer tests pass independently of OpenCU;
this procedure verifies the separately installed real engine and Windows tokens.

## Isolation and prerequisites

Use a disposable **non-administrator Windows account**, signed in at the
physical console (not RDP). This keeps its OpenCU manifest and test apps separate
from the normal user's installation. Leave the production wanctl install,
config, task/service and agent untouched. Arrange an idle period so production
controllers do not send desktop input during the test. Do not run `wanctl
install`, `service install`, `start`, `update` or `stop` against a production config.

Use a Mac with Go, Python 3 and SSH access from Windows. The relay below binds
only Mac loopback; Windows forwards it over SSH, so no unauthenticated LAN
listener or production relay is involved. The test token is intentionally
throwaway and has no production authority. Keep all commands scoped to the
explicit test binaries/config directories. Two Windows terminal windows are
needed, plus one for the disposable app; a helper process itself must show no
console window.

## Build and start the isolated relay (Mac, zsh)

```sh
cd /Users/e0_7/projects/wanctl-oss-worktrees/act-opencu
mkdir -p .acceptance/controller .acceptance/relay
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o .acceptance/wanctl-test.exe .
go build -o .acceptance/wanctl .
WANCTL_CONFIG_DIR="$PWD/.acceptance/relay" \
  WANCTL_TOKENS='opencu-acceptance-only:lab' WANCTL_AUTO_UPDATE=off \
  .acceptance/wanctl relay --addr 127.0.0.1:18080
```

Keep that terminal open. In another Mac terminal, in the same worktree:

```sh
export WANCTL_CONFIG_DIR="$PWD/.acceptance/controller"
export WANCTL_RELAY=ws://127.0.0.1:18080 WANCTL_TRANSPORT=ws
export WANCTL_TOKEN=opencu-acceptance-only WANCTL_AUTO_UPDATE=off
unset WANCTL_WORKSPACE
W="$PWD/.acceptance/wanctl"
"$W" label 'OpenCU isolated acceptance'
```

`WANCTL_AUTO_UPDATE=off` is explicit; these development builds also identify
as `dev`. No release build or installed production executable is replaced.

## Start the second agent (Windows, ordinary PowerShell)

Replace `MAC_USER` and `MAC_LAN_ADDRESS` with the SSH login and Mac address.
In the first Windows terminal, keep the tunnel running:

```powershell
ssh -N -L 18080:127.0.0.1:18080 MAC_USER@MAC_LAN_ADDRESS
```

In a second Windows terminal, under the disposable non-admin account:

```powershell
$lab = Join-Path $env:USERPROFILE 'wanctl-opencu-acceptance'
New-Item -ItemType Directory -Force $lab | Out-Null
Set-Location $lab
scp MAC_USER@MAC_LAN_ADDRESS:/Users/e0_7/projects/wanctl-oss-worktrees/act-opencu/.acceptance/wanctl-test.exe .
$env:WANCTL_CONFIG_DIR = Join-Path $lab 'agent-config'
$env:WANCTL_RELAY = 'ws://127.0.0.1:18080'
$env:WANCTL_TRANSPORT = 'ws'
$env:WANCTL_TOKEN = 'opencu-acceptance-only'
$env:WANCTL_AUTO_UPDATE = 'off'
.\wanctl-test.exe agent --name opencu-lab-user --yes --mode bypass
```

Keep its real terminal open: even in bypass mode it is the human approver
when OpenCU has been stopped. `--yes` only auto-trusts controllers on this
isolated relay. Record the fingerprint printed by this agent.

On the Mac, list the test relay and pin **that exact fingerprint**, after
comparing it with Windows. Resolve only the temporary device:

```sh
"$W" peers
# Substitute the temporary device ID shown by peers and its Windows fingerprint.
DEVICE='lab/DEVICE_ID_FROM_PEERS'
"$W" trust server --target "$DEVICE" --fingerprint 'SHA256:FINGERPRINT_FROM_WINDOWS'
# Use the real absolute Windows test directory printed by $lab, with its username.
"$W" workspace enter --target "$DEVICE" --root 'C:\Users\LAB_USER\wanctl-opencu-acceptance' > .acceptance/workspace.json
export WANCTL_WORKSPACE="$(python3 -c 'import json; print(json.load(open(".acceptance/workspace.json"))["workspace"])')"
"$W" workspace status
```

Expected: `state: open`, and the reference ends in `#w-` plus 32 hex digits.
The workspace reference, not each CLI process, identifies this conversation.

## Missing installation, install, discovery

Before installing OpenCU for this new Windows account:

```sh
"$W" act --discover
```

Expected: nonzero exit and JSON `code: OPENCU_NOT_INSTALLED`, with this exact
command and a message saying no administrator is needed and actions still work:

```powershell
irm https://github.com/Daily-AC/opencu/releases/latest/download/install.ps1 | iex
```

Run that command **in another ordinary PowerShell under the same Windows test
account**. Do not run it as SYSTEM or administrator. Then, on the Mac:

```sh
"$W" act --discover > .acceptance/discovery.json
python3 -m json.tool .acceptance/discovery.json
```

Expected: `serverInfo.version`, nonempty `instructions`, and tools with names,
descriptions, `inputSchema` and annotations. Check the actual installed schemas
before using the examples below; these parameter names were checked against
OpenCU's public MCP definitions on 2026-10-17. No tool name is built into wanctl.

## Disposable app, real observation and click

In a third Windows PowerShell, create an app with no documents or external side
effects. Keep the normal user's apps closed in this disposable login.

```powershell
Add-Type -AssemblyName System.Windows.Forms
$f = New-Object Windows.Forms.Form
$f.Text = 'wanctl OpenCU Acceptance'
$f.Width = 480; $f.Height = 260
$b = New-Object Windows.Forms.Button
$b.Text = 'Increment'; $b.SetBounds(30,30,180,40)
$l = New-Object Windows.Forms.Label
$l.Text = 'Count: 0'; $l.SetBounds(30,90,300,40)
$script:count = 0
$b.Add_Click({ $script:count++; $l.Text = "Count: $script:count" })
$f.Controls.Add($b); $f.Controls.Add($l)
[Windows.Forms.Application]::Run($f)
```

On the Mac, after the person agrees and leaves keyboard/mouse idle:

```sh
"$W" act --tool get_app_state \
  --arguments '{"app":"wanctl OpenCU Acceptance","includeScreenshot":true}' \
  -o .acceptance/state.json
```

Expected: text with the observed Increment control and Count: 0, a PNG content
item, `isError` absent/false, and any `_meta` preserved in `state.json`.
Extract the actual image without recompressing it:

```sh
python3 - <<'PY'
import base64, json
r = json.load(open('.acceptance/state.json'))
for i, c in enumerate(r['content']):
    if c['type'] == 'text': print(c['text'])
    elif c['type'] == 'image':
        open(f'.acceptance/state-{i}.png', 'wb').write(base64.b64decode(c['data']))
PY
```

Read the element index for Increment from that observation; do not guess it.

```sh
read 'BUTTON_INDEX?Observed Increment element index: '
"$W" act --tool click \
  --arguments "{\"app\":\"wanctl OpenCU Acceptance\",\"element_index\":$BUTTON_INDEX}" \
  -o .acceptance/click.json
```

Expected: the app visibly says Count: 1 and the returned state confirms it.
These are independent CLI invocations with one workspace. `PERSON_ACTIVE`
means stop and ask the person, not repeat the click automatically. A tool
`isError: true` gives a nonzero CLI exit while retaining its complete result.

To prove process reuse, in another Windows terminal use the test agent's PID
(from `Get-CimInstance Win32_Process`, filtering the test executable and its
`agent --name opencu-lab-user` command line):

```powershell
$agentProcess = Get-CimInstance Win32_Process | Where-Object {
  $_.Name -eq 'wanctl-test.exe' -and $_.CommandLine -match 'agent --name opencu-lab-user'
}
$helperProcess = Get-CimInstance Win32_Process | Where-Object {
  $_.ParentProcessId -eq $agentProcess.ProcessId -and $_.CommandLine -match '__desktop'
}
$peerProcess = Get-CimInstance Win32_Process | Where-Object {
  $_.ParentProcessId -eq $helperProcess.ProcessId
}
$agentProcess, $helperProcess, $peerProcess | Select-Object Name,ProcessId,ParentProcessId,SessionId,CommandLine
```

Record the helper and manifest-command PIDs, run another explicit observation,
and repeat this listing. Both PIDs must stay the same, in the console user's
nonzero session. Process Explorer can verify the helper and Node token have
Medium integrity, not High/System. No extra console window should appear.

## Esc stop, refusal and fresh approval (including bypass)

With explicit permission, hold only Shift over the disposable app:

```sh
"$W" act --tool press_key \
  --arguments '{"app":"wanctl OpenCU Acceptance","key":"Shift","duration":10}' \
  -o .acceptance/stopped.json
```

While it is in progress, the person at Windows presses physical **Esc**.
Expected: `USER_STOPPED` in the first text prefix and/or `_meta["opencu/error"]`,
`isError: true`, held input released. Do not replay the call.

Request a new observation only after asking the person:

```sh
"$W" act --tool get_app_state --arguments '{"app":"wanctl OpenCU Acceptance"}'
```

Expected: despite `--mode bypass`, the test agent terminal asks for approval
with summary `opencu get_app_state wanctl OpenCU Acceptance`. Refuse it once.
The reply must be `OPENCU_STOPPED`; no new helper/OpenCU PID appears. After the
person explicitly agrees, issue a new observation and approve **once** in the
agent terminal. Only then should a new helper/peer PID appear and state return.
A saved matching exec rule must behave the same way as bypass after another Esc.
Do not use workspace replacement, process restart or installation to clear stop.

For `PERSON_ACTIVE`, move the mouse during a separately authorized long hold.
Expected: the peer's original result and wanctl's ask-the-person/no-auto-retry note.

## Idle close, workspace close, transport and legacy checks

Leave this workspace idle for **10 minutes plus 5 seconds**, with no discovery
or tool calls. The recorded helper and peer PIDs must disappear. The workspace
may still say open; idle process cleanup does not close the logical workspace.
A new authorized observation creates new PIDs. If the connection was stopped
before the idle interval, that observation must still ask for fresh approval.

```sh
WANCTL_TRANSPORT=http "$W" act --tool get_app_state \
  --arguments '{"app":"wanctl OpenCU Acceptance","includeScreenshot":true}' \
  -o .acceptance/http-result.json
"$W" workspace exit
```

Expected: HTTP also returns full text/PNG/metadata; workspace exit closes the
helper and peer immediately. Calling act with the now-closed reference refuses
without starting a child. Finish this test before clearing the reference:

```sh
unset WANCTL_WORKSPACE
"$W" screenshot --target "$DEVICE" -o .acceptance/legacy.jpg > .acceptance/legacy.json
SHOT="$(python3 -c 'import json; print(json.load(open(".acceptance/legacy.json"))["snapshot"]["id"])')"
"$W" act --target "$DEVICE" --screenshot-id "$SHOT" \
  --actions '[{"type":"wait","ms":50}]' -o .acceptance/legacy-after.jpg
```

Expected: ordinary legacy desktop metadata/JPEG and completed action count,
not an OpenCU result. Sending both `--actions` and `--tool` must fail locally.

Inspect the temporary agent's `agent-config/logs/events.jsonl`. Only tool/app
summaries and outcome/approval metadata should be present, never argument text,
PNG data or MCP result content. Automated tests additionally cover forced peer
crash (unknown outcome, exactly one call) and concurrent legacy/OpenCU refusal;
repeat on Windows only with disposable input and the exact recorded test peer
PID, never by killing all Node/OpenCU processes.

## Exercise the session-0 LocalSystem launch separately

End the user-mode workspace and Ctrl-C **that test agent terminal** first.
Keep the SSH tunnel and the disposable account signed in at the physical
console. OpenCU must remain installed in that account's LOCALAPPDATA only.

In an administrator PowerShell, create a unique test task and directory:

```powershell
$s0 = 'C:\ProgramData\Wanctl-OpenCU-Acceptance'
if (Test-Path $s0) { throw 'Choose a fresh test directory instead of overwriting one' }
New-Item -ItemType Directory $s0 | Out-Null
icacls $s0 /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' '*S-1-5-32-545:(OI)(CI)RX'
# Substitute the disposable account's actual profile path.
Copy-Item 'C:\Users\LAB_USER\wanctl-opencu-acceptance\wanctl-test.exe' "$s0\wanctl-test.exe"
@'
$env:WANCTL_CONFIG_DIR = 'C:\ProgramData\Wanctl-OpenCU-Acceptance\config'
$env:WANCTL_RELAY = 'ws://127.0.0.1:18080'
$env:WANCTL_TRANSPORT = 'ws'
$env:WANCTL_TOKEN = 'opencu-acceptance-only'
$env:WANCTL_AUTO_UPDATE = 'off'
& 'C:\ProgramData\Wanctl-OpenCU-Acceptance\wanctl-test.exe' agent --name opencu-lab-s0 --yes --mode bypass *> 'C:\ProgramData\Wanctl-OpenCU-Acceptance\agent.log'
'@ | Set-Content -Encoding UTF8 "$s0\run.ps1"
$taskName = 'Wanctl-OpenCU-Acceptance-S0'
if (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) { throw 'Test task already exists' }
$action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument "-NoProfile -ExecutionPolicy Bypass -File $s0\run.ps1"
Register-ScheduledTask -TaskName $taskName -Action $action -User SYSTEM -RunLevel Highest
Start-ScheduledTask -TaskName $taskName
Get-Content "$s0\agent.log"
```

On the Mac, use `peers`, compare/pin **this second test agent's** fingerprint
from `agent.log`, enter a new workspace against its ID, and repeat discovery,
get_app_state and one observed Increment click. Its config/identity is separate
from both production and the first test agent. Use the same disposable app and
signed-in user's test directory as the workspace root.

Expected process evidence: the test agent has SessionId 0 and SYSTEM owner;
`__desktop-pipe` and the manifest command have the signed-in user's nonzero
SessionId, that user's owner and a non-elevated token. Discovery must succeed
without an OpenCU manifest in SYSTEM's profile. No signed-in console user or a
locked console must produce a clear refusal, not session-0 input. The named-pipe
path uses the existing PID and DACL checks; retain process/token evidence.

If testing Esc here, the next call must refuse `OPENCU_STOPPED`: this headless
test task has no human approver connected, so bypass cannot resume it. The
positive fresh-approval path is the real terminal test above. Do not wire any
production portal or approval phone into this isolated test.

## Cleanup and acceptance record

Exit all test workspaces first. Ctrl-C the user test agent/tunnel/relay terminals.
For the SYSTEM test, in the same admin PowerShell:

```powershell
# Targets only the config path and task created above.
$env:WANCTL_CONFIG_DIR = 'C:\ProgramData\Wanctl-OpenCU-Acceptance\config'
& 'C:\ProgramData\Wanctl-OpenCU-Acceptance\wanctl-test.exe' stop
Stop-ScheduledTask -TaskName 'Wanctl-OpenCU-Acceptance-S0'
Unregister-ScheduledTask -TaskName 'Wanctl-OpenCU-Acceptance-S0' -Confirm:$false
```

Check the recorded test helper/peer PIDs are gone before removing the temporary
Windows directories or disposable account. Close the disposable form. Retain
only the needed evidence, then delete the Mac `.acceptance` directory when done;
result files contain screenshots. Do not remove the production config/task.

Record: branch commit, Windows build, OpenCU version, each check's actual result,
user-mode and session-0 PID/owner/integrity evidence, exact stop code and refusal,
idle elapsed time, screenshot hashes, and failures. Compilation and a fake peer
do not establish real Windows acceptance. No production rollout is part of this
runbook or this change.
