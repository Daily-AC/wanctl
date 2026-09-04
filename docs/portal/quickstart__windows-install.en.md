# Install and join on Windows in one line

Run three lines in PowerShell — install the tool, set the instance addresses
(once), sign in and join:

```powershell
irm https://github.com/Daily-AC/wanctl/releases/latest/download/install.ps1 | iex
wanctl config set relay=https://relay.example.com portal=https://portal.example.com
wanctl
```

No OpenSSL needed — the installer verifies the release signature with
PowerShell's own cryptography interfaces, and writes nothing until it checks
out. Every Windows since 7 ships a PowerShell new enough for this.

Signing in opens the portal login in a browser (GitHub account) and gives you a
one-time code; paste it back into the terminal. After that:

```powershell
wanctl service install
```

This registers a scheduled task that starts at boot and keeps running after you
close the terminal window (the relay address goes straight into the task, so a
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
