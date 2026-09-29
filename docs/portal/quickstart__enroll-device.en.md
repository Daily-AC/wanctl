# Make a machine remotely controllable (Mac / Linux)

Run three lines on **the machine you want to control** — install the tool, set
the instance addresses (once), sign in and join:

```
curl -fsSL https://github.com/Daily-AC/wanctl/releases/latest/download/install.sh | sh
wanctl config set relay=https://relay.example.com portal=https://portal.example.com
wanctl start
```

A browser opens the portal login (GitHub account) and shows a one-time code.
Paste it back into the terminal and press enter. The service then moves into
the background: `wanctl stop` stops it, `wanctl status` shows how it is doing.
Installing needs **no token at all**.

> Signing in to the portal at all assumes you are already part of this
> deployment: the first user to log in becomes the administrator, and everyone
> after that is admitted by an administrator: invited by GitHub username, or
> approved after sending a request from the page that greets them
> (see "Invites, friends and sharing").
>
> You can skip the second line and run `wanctl start` on its own — the first run asks
> in the terminal which relay to use, this deployment's or the project's hosted
> instance, and saves your answer. `wanctl config` shows and changes what you
> set, and `WANCTL_RELAY`/`WANCTL_PORTAL` still override it for one run. Nothing
> scripted is ever blocked by the question: with no terminal, or with
> `WANCTL_NO_PROMPT=1`, the command prints the `wanctl config set` line and exits.

Both the tool and the install script come from the project's
[GitHub Releases](https://github.com/Daily-AC/wanctl/releases): the installer
verifies the signed release manifest, checks each binary's size and hash, and
only then writes anything to disk (with the system's own `openssl` — macOS's
LibreSSL works too). The release source and your relay are independent of each
other, so a relay having a bad day does not stop an install.

## Install it as a service to survive a reboot

An agent started by `wanctl start` survives closing the terminal, but not
logging out or rebooting. For a machine you want to control long term, sign in
once and then install the service (stop the agent `wanctl start` launched
first, so the two do not fight over one config directory):

```
wanctl stop
wanctl service install
wanctl service status
```

`service install` sets up a launchd agent on macOS, a systemd user service on
Linux and a scheduled task on Windows, and restarts the agent if it exits
unexpectedly. It writes the relay address and transport you configured into the
service unit, so a reboot does not depend on environment variables
(`--relay` / `--transport` set them explicitly). On macOS and Windows it starts
once a user logs in, so an unattended machine needs automatic login. On Linux it
also tries `loginctl enable-linger`; if that works the agent comes up at boot
without a login, and if not it tells you to run it again with sudo. To retire
the service later, run `wanctl service uninstall`.

Windows machines: see the [next page](#docs/windows-install).
