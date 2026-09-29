# Install and join on Windows in one line

Run three lines in PowerShell — install the tool, set the instance addresses
(once), sign in and join:

```powershell
irm https://github.com/Daily-AC/wanctl/releases/latest/download/install.ps1 | iex
wanctl config set relay=https://relay.example.com portal=https://portal.example.com
wanctl start
```

No OpenSSL needed — the installer verifies the release signature with
PowerShell's own cryptography interfaces, and writes nothing until it checks
out. Every Windows since 7 ships a PowerShell new enough for this.

Signing in opens the portal login in a browser (GitHub account) and gives you a
one-time code; paste it back into the terminal. After that:

```powershell
wanctl service install
```

This registers a scheduled task that starts when you log in, runs as you, and
keeps running after you close the terminal window (the relay address goes straight into the task, so a
reboot does not depend on environment variables). `wanctl status` shows how it
is doing, `wanctl stop` stops it.

## About trust

The script and the binaries come from the project's
[GitHub Releases](https://github.com/Daily-AC/wanctl/releases), independent of
your relay; the script verifies the release signature, the size and the SHA-256
before writing anything. If you mirror releases inside your own network, set
`$env:WANCTL_RELAY` before the install command and it pulls from that relay's
/dl instead.

## Two known traps on Windows

**A space between every character of the output.** Native Windows tools
(`wsl.exe` above all) emit UTF-16LE; decoded as the OEM code page, the zero
bytes survive as separators. Current builds handle this. If you still see it,
the agent is an old one — run `wanctl update`.

**Quotes get eaten.** The login shell on Windows is PowerShell, where `$` and
quotes are easily swallowed across several layers of escaping. Do not fight it
for a complex command: send the script over with `wanctl push` and run that.

## Running a command as the signed-in user when the agent runs as SYSTEM

If you installed the agent as a SYSTEM service (`wanctl service install` does
not), commands run as SYSTEM too. A per-user installer then puts things in the
wrong place, or leaves SYSTEM-owned files in the user's profile that the user
cannot change or delete later.

The way round it is a one-off scheduled task that runs as the signed-in user.
Save the block below as `run-as-user.ps1`, set `$User` and the commands between
the two `'@` lines, and send it:
`wanctl exec --target DEVICE --script run-as-user.ps1`. The commands run in that
user's desktop session with their ordinary rights (not elevated); environment
variables, HKCU and the owner of any file they create are all that user's. The
output and exit code come back as they are; past `$TimeoutSec` (10 minutes by
default) the commands are ended together with their child processes; the task
and the temporary files are removed afterwards.

```powershell
# run-as-user.ps1: run the block below as a signed-in user, from an agent running as SYSTEM.
$User = 'alice'          # `quser` lists who is signed in; MACHINE\alice works too
$TimeoutSec = 600
$Payload = @'
whoami
$env:LOCALAPPDATA
'@

$ErrorActionPreference = 'Stop'
$sid = (New-Object Security.Principal.NTAccount $User).Translate([Security.Principal.SecurityIdentifier]).Value
$User = (New-Object Security.Principal.SecurityIdentifier $sid).Translate([Security.Principal.NTAccount]).Value
if (-not (Get-Process explorer -IncludeUserName | Where-Object UserName -eq $User)) {
    [Console]::Error.WriteLine("$User is not signed in; the task would never start")
    exit 1
}
$home_ = (Get-ItemProperty "HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList\$sid").ProfileImagePath
# Stage in the user's own Temp: a script SYSTEM leaves in C:\Windows\Temp is unreadable to the user.
$id = 'wanctl-as-' + [guid]::NewGuid().ToString('N').Substring(0, 8)
$dir = Join-Path $home_ "AppData\Local\Temp\$id"
New-Item -ItemType Directory $dir | Out-Null
"`$PID | Set-Content `"$dir\pid.txt`"; [Console]::OutputEncoding = [Text.Encoding]::UTF8`r`n" + $Payload | Set-Content "$dir\payload.ps1" -Encoding UTF8
@"
@powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0payload.ps1" > "%~dp0out.txt" 2>&1
@(echo %ERRORLEVEL%)> "%~dp0rc.txt"
"@ | Set-Content "$dir\run.cmd" -Encoding ASCII

# Interactive logon type: runs in the user's desktop session with their own token, no password needed.
# conhost --headless keeps a console window from popping up on that desktop.
$action = New-ScheduledTaskAction -Execute 'conhost.exe' -Argument "--headless cmd.exe /c `"$dir\run.cmd`"" -WorkingDirectory $home_
$principal = New-ScheduledTaskPrincipal -UserId $User -LogonType Interactive -RunLevel Limited
# Task defaults refuse to start on battery, which would leave a laptop waiting for the timeout.
$settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
Register-ScheduledTask -TaskName $id -Action $action -Principal $principal -Settings $settings | Out-Null
try {
    Start-ScheduledTask -TaskName $id
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while (-not (Test-Path "$dir\rc.txt") -and (Get-Date) -lt $deadline) { Start-Sleep -Milliseconds 300 }
    if (-not (Test-Path "$dir\rc.txt")) {
        # Stopping the task only ends conhost; take down the payload's own process tree.
        if (Test-Path "$dir\pid.txt") { taskkill /PID (Get-Content "$dir\pid.txt") /T /F | Out-Null }
        Stop-ScheduledTask -TaskName $id
        Start-Sleep 1
        [Console]::Error.WriteLine("timed out after $TimeoutSec s (is $User signed in?)")
        $rc = 124
    } else {
        Get-Content "$dir\out.txt" -Encoding UTF8
        $rc = [int](Get-Content "$dir\rc.txt")
    }
} finally {
    Unregister-ScheduledTask -TaskName $id -Confirm:$false
    Remove-Item $dir -Recurse -Force -ErrorAction SilentlyContinue
}
exit $rc
```

Three limits:

- The output comes back in one piece when the commands finish, not as they run.
- The user has to be signed in already; if not, the script says so and stops.
- Ctrl-C part way through stops only the outer script. The commands in the
  user's session run to the end, and the task and the temporary folder are left
  behind. These two lines clean them up:

```powershell
Get-ScheduledTask wanctl-as-* | Unregister-ScheduledTask -Confirm:$false
Remove-Item "C:\Users\*\AppData\Local\Temp\wanctl-as-*" -Recurse -Force
```
