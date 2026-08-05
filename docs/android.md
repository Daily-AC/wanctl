# Android devices

wanctl runs on Android as a **controlled device** — you drive an Android phone
or tablet from a terminal the same way you drive a Linux box. The controller
side (running `wanctl exec` *from* Android) is not a separate product: the same
binary has the controller subcommands, they just have not been exercised there.

Shipped artifact: `wanctl-android-arm64`. There is no 32-bit or x86 Android
build; every Android device made in the last decade is arm64.

Verified end-to-end on 2026-08-05 against a vivo PA2353 (Android 13, arm64) and
an Android 16 arm64 emulator, over the production relay:
`exec` with streaming output and real exit codes, `push`/`pull` (512 KB binary
round-trip, SHA-256 identical), TOFU pinning both ways, and detached `start` /
`stop` / `status`.

## Install

### In Termux (recommended)

```sh
pkg install openssl-tool          # the installer verifies a signature with it
curl -fsSL https://wanctl-relay.***REMOVED***.***REMOVED***.com/dl/install.sh | sh
wanctl                            # Feishu login, then run detached
```

The installer detects Android and puts the binary in `$PREFIX/bin`, which is on
PATH and is executable — most other writable locations on Android are not.

### Without installing an app (adb)

Useful for a device you already have on USB, and for a device where you do not
want a terminal app at all.

```sh
adb push wanctl-android-arm64 /data/local/tmp/wanctl
adb shell chmod 755 /data/local/tmp/wanctl
adb shell /data/local/tmp/wanctl        # login + detached agent
```

`/data/local/tmp` is the one place the adb shell user can both write and
execute. The agent keeps its identity and token in `/data/local/tmp/.wanctl`.

## What is different on Android, and why

Five things behave differently here than on Linux. Each is handled by the
binary; they are documented because they explain the layout and the failure
modes you might otherwise misread.

**DNS.** Android has no `/etc/resolv.conf` — resolution goes through `netd`,
reachable only via bionic's libc. wanctl ships CGO-free static binaries, so the
Go resolver would fall back to `127.0.0.1:53` and every lookup would fail with
`connection refused` before a single packet reached the relay. The Android build
points the resolver at explicit nameservers instead (AliDNS, Google, Cloudflare,
rotating so a retry reaches a different operator). `net.dns1` is not consulted:
netd stopped publishing it around Android 8 and it reads empty on modern
devices.

Override it when the relay is on a private or split-horizon zone:

```sh
export WANCTL_DNS=10.0.0.53,10.0.0.54     # comma-separated, :53 assumed
```

A device that does have `/etc/resolv.conf` (a proot distro, a rooted setup) is
left alone and uses the stock resolver. Termux's own
`$PREFIX/etc/resolv.conf` is read when present.

**Config directory.** In Termux, `$HOME` is real and the config lands in
`$HOME/.config/wanctl` as usual. Under an adb shell `HOME=/`, which is
read-only, so the agent falls back to the first writable candidate:
`$TMPDIR/wanctl`, then `.wanctl` next to the binary, then
`/data/local/tmp/.wanctl`. `WANCTL_CONFIG_DIR` overrides all of it.

**Shell.** `/bin/sh` only exists on Android 11+ (as a symlink into
`/system/bin`), so it cannot be the default. The agent prefers
`$PREFIX/bin/sh` in Termux — a session in the system shell would have none of
your Termux tools — and falls back to `/system/bin/sh`, which every Android
build has.

**File transfers.** Android grants the shell user traverse-only access to the
directories leading anywhere useful (`/data` is `drwxrwx--x system:system`), and
`os.Root` must *open* each component on the way down. Binding a transfer to the
volume root therefore failed with `openat data/local/tmp: permission denied`
even though the destination was writable. On Android an otherwise-unconstrained
transfer binds to the target's own directory instead. This is narrower than the
volume root, not wider, and symlinks at the destination are still refused.

**Device name.** Every Android device reports its hostname as `localhost`, so
the default name would be both meaningless and a collision between any two
devices in a namespace. The agent asks the property service instead
(`ro.product.marketname`, then `ro.product.model`) and registers as e.g.
`pa2353`. Two devices of the same model still collide — pass `--name`.

## Keeping the agent alive

This is the weak spot, and Android is the reason: there is no service manager an
unprivileged process can install itself into. `wanctl service install` therefore
refuses on Android rather than reporting a success it did not achieve.

`wanctl start` detaches the agent and it survives the terminal closing — this
was verified with the tablet locked. What it does not survive is a reboot, and
Android may kill it under memory pressure.

For something sturdier, Termux is the path: `termux-wake-lock` keeps Android
from dozing the process, the separate **Termux:Boot** app runs `~/.termux/boot/`
scripts at startup, and `pkg install termux-services` adds a runit supervisor.
**None of these three has been verified by wanctl** — they are the documented
Termux mechanisms, not a tested wanctl integration. Treat this section as a
starting point rather than a recipe.

## Not yet verified

- **Self-update on Android.** `wanctl update` resolves `android/arm64` from the
  signed manifest like any other platform, but no release has shipped that
  artifact yet, so the download-and-swap path has not run on a device.
- **Anything inside Termux.** Every check above was run through an adb shell.
  The Termux-specific branches (`$PREFIX` paths, `$PREFIX/bin/sh`, the Termux
  installer destination) are exercised by unit tests and by simulating `PREFIX`,
  not by a real Termux session.
- **Termux:Boot / termux-services**, as above.
