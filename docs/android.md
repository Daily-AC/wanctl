# Android devices

wanctl runs on Android as a **controlled device** — you drive an Android phone
or tablet from a terminal the same way you drive a Linux box. The controller
side (running `wanctl exec` *from* Android) is not a separate product: the same
binary has the controller subcommands, they just have not been exercised there.

Shipped artifact: `wanctl-android-arm64`. There is no 32-bit or x86 Android
build; every Android device made in the last decade is arm64.

Verified end-to-end against the production relay on a vivo PA2353 (Android 13,
arm64) and an Android 16 arm64 emulator — `exec` with streaming output and real
exit codes, `push`/`pull` (512 KB binary round-trip, SHA-256 identical), TOFU
pinning both ways, detached `start` / `stop` / `status`, and the agent surviving
a locked screen. Run through an adb shell on 2026-08-05 and, separately, from
inside Termux on 2026-08-06 — the two environments fail differently and neither
substitutes for the other.

## Install

### In Termux (recommended)

```sh
pkg install openssl-tool          # the installer verifies a signature with it
curl -fsSL https://wanctl-relay.***REMOVED***.***REMOVED***.com/dl/install.sh | sh
wanctl                            # Feishu login, then run detached
```

The installer detects Android and puts the binary in `$PREFIX/bin`, which is on
PATH. (Until a release ships the `android/arm64` artifact this path cannot work
yet — see "Not yet verified".)

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

**Shell.** Sessions run in `/system/bin/sh` (mksh), the one shell present on
every Android build. `/bin/sh` exists only from Android 11 on (a symlink into
`/system/bin`), so it cannot be the default.

Termux's own `$PREFIX/bin/sh` is deliberately not used, even in Termux: the
agent physically cannot start it. Android refuses `exec` of a file in an app's
private data directory from the `untrusted_app` domain, and Termux only manages
it because `libtermux-exec.so`, preloaded into its shell, rewrites every
`execve` to go through the dynamic linker — a CGO-free Go binary loads no such
library, so the call comes back `permission denied`.

You lose nothing by this. A `/system/bin/sh` session inherits Termux's `PATH`,
and Termux binaries started *by that shell* run normally, because mksh performs
those execs rather than wanctl. Verified over the relay from a Termux-hosted
agent: `git --version` → 2.52.0, `python --version` → 3.12.12, both Termux's.

**File transfers.** Android grants the shell user traverse-only access to the
directories leading anywhere useful (`/data` is `drwxrwx--x system:system`), and
`os.Root` must *open* each component on the way down. Binding a transfer to the
volume root therefore failed with `openat data/local/tmp: permission denied`
even though the destination was writable. On Android an otherwise-unconstrained
transfer binds to the target's own directory instead. This is narrower than the
volume root, not wider, and symlinks at the destination are still refused.

**Argument vector (Termux only).** Go's Android target is always dynamic PIE, so
the binary names `/system/bin/linker64` as its ELF interpreter, and Termux's exec
interceptor re-runs such programs through the linker explicitly — which prepends
the program's resolved absolute path to `argv`. `wanctl version` arrives as
`[wanctl, /data/data/com.termux/files/usr/bin/wanctl, version]`, shifting every
argument one slot late and making the binary inert
(`unknown command "/data/data/com.termux/..."`). The Android build detects the
duplicate — argv[1] being an absolute path to a regular executable whose base
name matches argv[0]'s — and drops it.

Nothing needs configuring. It is documented because the symptom is baffling if
you meet it, and because **v0.1.7 got this wrong**: its check required argv[0]
to resolve as a file, which a bare `wanctl` off `PATH` does not, so the
installed binary was unusable in Termux. Fixed in v0.1.8. On v0.1.7, calling it
by an explicit path (`$PREFIX/bin/wanctl version`) works around it.

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

## Pairing an Android device

Two things changed in the relay/agent since the first Android round and both
show up here:

- **The controller must identify itself.** A device refuses a pairing request
  from a controller with no label, so set one once:
  `wanctl label "<who you are>"` (or `WANCTL_LABEL`). Already-paired
  controllers and agents started with `--yes` are unaffected — which is most
  Android testing, so you may never see it.
- **The portal fingerprint seeds itself.** Enrolling a device (`wanctl` with no
  arguments) now writes the portal identity into the device's
  `portal_admins.json`, so the web console can reach an Android device without
  anyone passing `--portal-fps` by hand. Compare the printed fingerprint with
  the one on the enrollment page.

A device's activity log now also records *refused* connections
(`rejected:unpaired`, `:unlabeled`, `:capability`, …) with a reason, which is
the fastest way to see why a phone is ignoring you.

## Not yet verified

- **Self-update on Android.** `wanctl update` resolves `android/arm64` from the
  signed manifest like any other platform, but no release has shipped that
  artifact yet, so the download-and-swap path has not run on a device. That
  unblocks once this branch's `scripts/build-release.sh` change is on main and
  a release is cut.
- **The one-line installer on Android**, for the same reason: there is no
  `android/arm64` entry in the published manifest to install. Its platform
  detection and destination logic were exercised on the device by running the
  same shell in a real Termux session and over adb.
- **Termux:Boot / termux-services** — the documented Termux mechanisms for
  surviving a reboot, not a tested wanctl integration.
