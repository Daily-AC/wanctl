# Make a machine remotely controllable (Mac / Linux)

Run three lines on **the machine you want to control** — install the tool, set
the instance addresses (once), sign in and join:

```
curl -fsSL https://github.com/Daily-AC/wanctl/releases/latest/download/install.sh | sh
wanctl config set relay=https://relay.example.com portal=https://portal.example.com
wanctl
```

A browser opens the portal login (GitHub account) and shows a one-time code.
Paste it back into the terminal and press enter. The service then moves into
the background: `wanctl stop` stops it, `wanctl status` shows how it is doing.
Installing needs **no token at all**.

> Signing in to the portal at all assumes you are already part of this
> deployment: the first user to log in becomes the administrator, and everyone
> after that needs an invite code from an administrator, redeemed after login
> (see "Invites, friends and sharing").
>
> You can skip the second line and run `wanctl` on its own — the first run asks
> for both addresses in the terminal. `wanctl config` shows and changes what you
> set, and `WANCTL_RELAY`/`WANCTL_PORTAL` still override it for one run.

Both the tool and the install script come from the project's
[GitHub Releases](https://github.com/Daily-AC/wanctl/releases): the installer
verifies the signed release manifest, checks each binary's size and hash, and
only then writes anything to disk (with the system's own `openssl` — macOS's
LibreSSL works too). The release source and your relay are independent of each
other, so a relay having a bad day does not stop an install.

To start at boot, run `wanctl service install` — it writes the relay address and
transport you configured into the service unit, so a reboot does not depend on
environment variables (`--relay` / `--transport` set them explicitly).

Windows machines: see the [next page](#docs/windows-install).
