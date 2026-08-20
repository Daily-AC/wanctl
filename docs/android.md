# Android devices

wanctl runs on Android as a **controlled device** — you drive an Android phone
or tablet from a terminal the same way you drive a Linux box. The controller
side (running `wanctl exec` *from* Android) is not a separate product: the same
binary has the controller subcommands, they just have not been exercised there.

There are two ways to run it, and they are genuinely different products:

| | APK (recommended) | Termux |
|---|---|---|
| survives a reboot | yes, `BOOT_COMPLETED` | no |
| survives the app closing | yes, foreground service | `wanctl start` detaches, until Android kills it |
| `wanctl update` | no — updates ship as APKs | yes |
| needs another app installed | no | Termux, from F-Droid |
| workarounds it depends on | none | four, on Termux internals |

Shipped artifacts: `wanctl-android-arm64.apk` and `wanctl-android-arm64`. There
is no 32-bit or x86 Android build; every Android device made in the last decade
is arm64.

## The app

```sh
# from the relay, on the device itself
https://relay.example.com/dl/wanctl-android-arm64.apk
```

Install it, open it, tap **登录**, and follow the Feishu enrollment the same way
every other platform does. Then turn on **启用 wanctl**. The agent runs as a
foreground service — there is a permanent notification, which is the deal
Android offers: the system keeps the process alive and the user always knows.

The UI is five switches and four buttons:

- **加入电池优化白名单** is **not optional**, whatever the wording suggests.
  Android permits a background foreground-service start only for an exempt app;
  the successful start after a reboot logs, in as many words,
  `am_foreground_service_start: … SYSTEM_ALLOW_LISTED`. Without the exemption
  the agent will not come back on its own. The service additionally holds a
  partial wake lock, which is what `termux-wake-lock` did for the Termux route.
  **On a Chinese OEM ROM this is only half of it — see the next section, which
  is the difference between a device that works and one that does not.**
- **开机自启** (on by default) restarts the agent after a reboot and after the
  app updates itself. See "Coming back after a reboot" below for what it does
  and does not guarantee.
- **自动信任新控制端** is **off**, and should stay off. With it off, an unknown
  controller's pairing request is raised to the portal web console for a human
  decision, which works on a device with no keyboard. With it on, anything
  holding a namespace token pairs silently.
- **自动放行所有命令** is **off**, and turning it on is a real decision. Off,
  the agent runs in wanctl's `normal` policy mode: a command matching no rule is
  refused until a human approves it in the portal console. That is right for a
  device someone watches and unusable for an unattended one — and an APK has no
  shell to type `wanctl rules` into, so this switch is the only way to say so.
  Expect `command denied by device policy` until you make the choice.
- **设备名** defaults to the model name (`pa2353`). Two devices of the same
  model collide; set one here.

### The OEM gates, which decide whether any of this works

Android's own controls are not the ones that matter most here. On a Chinese OEM
ROM there are two more, both invisible to the app, and the second one is the
difference between a working device and a useless one.

**自启动 / autostart** decides whether `BOOT_COMPLETED` is delivered at all. On
the PA2353 wanctl was granted it automatically; do not count on that. Check
设置 → 应用与权限 → 自启动 if the agent never returns after a reboot.

**后台耗电管理 / background power** decides whether the app is *frozen* while
the screen is off — and its default freezes it. Measured on the PA2353 with the
battery-optimization exemption granted, autostart on, a foreground service
running and a partial wake lock held:

| | freezer cgroup | process state | reachable |
|---|---|---|---|
| 智能控制后台耗电 (default) | `freezer:/frozen` | `D`, all 11 threads | no, within ~2 min of screen-off |
| 允许后台高耗电 | `freezer:/` | `S` | yes, 8 min and counting |

Frozen means the poll loop is not running at all — no errors, no retries, no log
lines, and nothing the app can do about it. A foreground service does not
prevent it. `PARTIAL_WAKE_LOCK` does not prevent it. The AOSP battery exemption
does not prevent it.

设置 → 电池 → 后台耗电管理 → wanctl → **允许后台高耗电**. Termux is set this way
on the same device, which is why the Termux route's locked-screen test passed.
The app's 电池 button links to the equivalent path for OPPO, Xiaomi and Huawei.

### Pairing an APK device, in order

The gates fire one at a time and each has a different fix, so the first three
`wanctl exec` attempts fail differently:

1. **The controller must confirm the device's identity.** `wanctl exec` prints a
   `wanctl trust server --target … --fingerprint …` line. Compare that
   fingerprint against the one the app shows under 指纹 — that comparison is the
   whole point, so do it with your eyes rather than pasting.
2. **The device must trust the controller.** Unless 自动信任新控制端 is on, the
   agent refuses and prints a portal URL, valid five minutes, for the device
   owner to click.
3. **The command must pass policy.** See 自动放行所有命令 above.

### Where files go

`wanctl push` and `pull` must target a directory the *app* can write, which
means somewhere under `/data/user/0/dev.wanctl.agent/`. `/data/local/tmp` is
writable by the adb shell user and **not** by an app — pushing there fails with
`permission denied` even though the same path works for an adb-pushed binary.
Exec sessions start in the app's own files directory, so a relative path lands
somewhere sensible.

### Built-in battery state

The APK agent exposes battery state without sending a command through Android's
restricted app shell:

```sh
wanctl exec --target phone -- battery
```

The command writes one JSON object to stdout:

```json
{"level":76,"status":"charging","plugged":"usb","temperature_c":31.4,"health":"good","updated_at":"2026-08-13T18:42:03Z","age_seconds":12}
```

`level` is a percentage. `status` is `charging`, `discharging`, `full`, `not
charging`, or `unknown`; `plugged` is `ac`, `usb`, `wireless`, `none`, or
`unknown`. `temperature_c` is Celsius, `health` is Android's normalized battery
health, and `updated_at` is the app collector's UTC timestamp. `age_seconds` is
computed by the Go agent when it answers.

The Java service writes the source snapshot atomically to
`files/state/device.json` when it starts and whenever Android sends a battery
change broadcast. The Go child receives that file's absolute path through its
environment. A missing, damaged, or more-than-10-minute-old snapshot is reported
as unavailable instead of being returned as current data. The verb is available
only when the agent is launched by the wanctl APK; other platforms return a
clear Android-only error. Normal exec policy still applies before the verb runs.

### Elevation: making the rest of adb work

Everything above happens inside the app sandbox, and most of `adb`'s surface is
not reachable from there. `pm`, `am`, `input`, `screencap`, `dumpsys`,
`settings`, `wm` and `svc` need uid 2000 (`shell`) or uid 0; the agent is an
ordinary app uid in `untrusted_app`. This is not a wanctl limitation to be
worked around one verb at a time — it is the platform's, and it is why the
built-in `battery` verb had to collect its data in Java.

**提权通道** in the app opens a channel that crosses that line. It is off by
default and it is a separate decision from every other switch here:

```sh
wanctl exec --target phone --elevate -- pm list packages -3
wanctl exec --target phone --elevate --via su -- dumpsys battery
```

Three channels exist; the agent probes them in order and uses the first
available one. `--via` pins one, and pinning an unavailable channel is an error
rather than a downgrade — a command that ran unprivileged after you asked for
root is a wrong answer wearing a right answer's clothes.

| channel | needs | survives a reboot |
|---|---|---|
| `su` | a rooted device | **yes** |
| `adb` | Developer options → Wireless debugging | no — Android clears it on boot |

Only `su` keeps working with nobody touching the device. A phone that must stay
controllable unattended after a power cut should be rooted, or should not depend
on elevation.

A third channel, Shizuku, was planned and cut on 2026-08-14: it reaches the same
uid 2000 the `adb` channel already reaches, it is itself started by wireless
debugging, and it asks the device's owner to install and re-start a second app.
`--via shizuku` says so rather than reporting an unknown channel.

#### Pairing, for a phone with no root and no cable

Android 11+ can hand out shell access over Wi-Fi without a computer, and the
agent can take it. On the device: 设置 → 开发者选项 → **无线调试** → **使用配对码
配对设备**. That screen shows a port and six digits. Then, from the controller:

```sh
wanctl exec --target phone -- adb-pair 37129 314159
```

The verb runs **on the device** — only it can reach the pairing port its own
adbd opened — and it needs no elevation, because setting up the elevation
channel with a command that itself requires elevation would be a loop with no
entry point. All it does is connect to 127.0.0.1, which an app sandbox may
already do.

Two ports appear on that screen and they are not the same listener: use the one
next to the pairing code, not the one on the wireless-debugging screen. A
`connection refused` here is almost always that mix-up.

What happens underneath, because it is not obvious: the pairing code alone is
not the SPAKE2 password. AOSP appends 64 bytes of TLS exported keying material
to it, binding the exchange to that specific TLS session so a relayed pairing
cannot be stolen. wanctl does the same — including the detail that the exporter
label is `"adb-label\0"` **with** its trailing NUL, because AOSP passes
`sizeof()` as the label length. Dropping that byte produces perfectly valid
keying material that simply never matches the device's.

Pairing is persistent: the key lands in `/data/misc/adb/adb_keys` and survives
reboots. Wireless debugging itself does not — Android turns it off on boot —
so after a restart the switch has to be flipped again, but the code does not
have to be re-entered.

The **connect** port is not stable either, and it is not the pairing port: the
number under IP 地址和端口 changes when wireless debugging is re-enabled and
after a re-pair (measured on a PGBM10: 37819 → 41031). The app does not ask you
for it — it watches mDNS for `_adb-tls-connect._tcp` and hands what it finds to
the agent through the same state file the battery verb uses. Only its own
device's advertisement counts: every phone on the same Wi-Fi with wireless
debugging on publishes that service, so the resolved address is checked against
this device's own addresses before the port is believed.

That discovery is Java (`NsdManager`) because mDNS on Android is a framework
service, and the app holds a `MulticastLock` while it watches — Wi-Fi filters
multicast in hardware when nothing does. Watching starts and stops with the
**提权通道** switch: a device whose owner has not turned elevation on is not
listening for the port that would enable it.

`WANCTL_ADB_PORT` still overrides everything, which is what the Termux and
adb-shell routes need: they have no framework to ask.

#### The adb channel needs a human once, and cannot be automated past that

The first time the agent connects to its own adbd, the device raises **允许 USB
调试吗？** showing wanctl's key fingerprint. Until someone accepts it, the
channel reports:

```
elevation channel "adb" is not available: adbd on port 5555 is waiting for
someone to allow wanctl's key on the device screen
```

That prompt cannot be answered from software. Measured on a Xiaomi Mi 10 Ultra
(Android 11) on 2026-08-14, `input tap` against it fails with

```
java.lang.SecurityException: Injecting to another application requires INJECT_EVENTS permission
```

which is the platform working as designed: if a program could tap that button,
any program could grant itself shell access. So enabling this channel always
costs one deliberate action by whoever is holding the phone — worth knowing
before planning an unattended rollout.

Note also what adbd does while it waits: **nothing**. It does not re-issue a
token and does not close the socket. A client that simply waits sees a bare
`i/o timeout` and no hint that a dialog is open, which is why the agent gives
up after a short grace period and reports the dialog instead.

**Elevated commands are their own policy class.** 自动放行所有命令 does not
cover them: that switch says "this device is unattended", not "hand out root",
so an elevated command on a bypass-mode device is still refused until it has a
rule of its own or a human approves it in the portal. An `exec` rule never
authorizes the elevated form of the same command either. The event log records
which channel ran each one, so `wanctl logs` can answer *what has run as root on
this phone*.

With the switch off, the channels are not even probed — a rooted device raises
no root-manager consent dialog for a feature nobody turned on.

### Verbs

The elevation channel is the whole feature; these are shorthand for the parts of
the adb surface that are awkward over a pipe. Each is one shell command, run
through the same channel, gated by the same elevated policy class, recorded in
the same audit log — nothing here does anything you could not type by hand.

```sh
wanctl screenshot phone -o shot.png       # implies --elevate; "-o -" writes stdout
wanctl exec --target phone --elevate -- input tap 540 1200
wanctl exec --target phone --elevate -- input text 'hello world'
wanctl exec --target phone --elevate -- app list -3
wanctl exec --target phone --elevate -- app uninstall com.example
wanctl exec --target phone --elevate -- settings get global adb_wifi_enabled
wanctl exec --target phone --elevate -- prop get ro.product.model
wanctl exec --target phone --elevate -- logcat
```

| verb | becomes | why it is a verb |
|---|---|---|
| `screenshot` | `screencap -p` | PNG on stdout; the controller writes the file |
| `input tap/swipe/text/key` | `input …` | arguments validated, and `text` stays one word |
| `app list/info/install/uninstall/start/stop/clear` | `pm`, `am`, `monkey`, `dumpsys` | one noun over four tools |
| `settings get/put` | `settings …` | the namespace is a closed set of three |
| `prop get/set` | `getprop`/`setprop` | — |
| `logcat` | `logcat -d -v time -t 200` | defaults to a dump: a follow never returns over exec |

Every argument is quoted before it becomes shell source that runs as root, so a
package name containing `; rm -rf /` arrives as a package name. A command
containing an unquoted expansion (`$(…)`, `$VAR`, a pipe) is not treated as a
verb at all and goes through as an ordinary elevated command — guessing at it
would be worse than letting the shell do what the shell does.

`wanctl screenshot` buffers the image and checks the PNG magic before writing,
so a failed capture leaves no truncated file that looks real.

**su is not one program.** Magisk, KernelSU and APatch take `su -c CMD`; AOSP's
own su, on userdebug builds and emulator images, rejects that outright and wants
`su root sh -c CMD`. The probe tries both and keeps whichever answered as root,
so neither family has to be detected by name. Measured on an android-29
`google_apis` emulator image, 2026-08-14:

```
su -c id            → su: invalid uid/gid '-c'
su root sh -c id    → uid=0(root) … context=u:r:su:s0
```

Verified on that image the same day: with the switch off the channel is listed
and refused with the switch as the reason; with it on, `su` probes to `uid=0`
and `settings get global adb_wifi_enabled` — the command that throws for an app
uid on the PGBM10 — returns normally.

### Verified, and on what

On a **Xiaomi Mi 10 Ultra** (M2007J1SC, Android 11, Magisk) and an **android-29
emulator**, both driven from a Mac over a live relay, 2026-08-14:

| | Mi 10 Ultra (Magisk) | emulator (AOSP su) |
|---|---|---|
| `exec -- id` | `uid=2000(shell)` | `uid=2000(shell)` |
| `exec --elevate -- id` (auto) | `uid=0(root)` `u:r:magisk:s0` | `uid=0(root)` `u:r:su:s0` |
| `exec --elevate --via adb -- id` | `uid=2000(shell)` `u:r:shell:s0` | — |
| exit code through the adb channel | 42 → 42 | — |
| `screenshot` | 1080×2340 PNG | 1080×2280 PNG |
| `settings` / `prop` / `app list` | all return real data | all return real data |
| event log | `"via":"su"`, `"via":"adb"` | `"via":"su"` |

Both su invocation forms were exercised for real: Magisk's `su` took `-c`, the
emulator's AOSP `su` refused it and needed `su root sh -c`. A build that guessed
one form would have reported "not rooted" on one of these two devices.

The agent under test ran from an adb shell, so its *unelevated* uid is 2000
there rather than the app uid an installed APK has. That does not affect what
the elevated channels prove — `su` reaching uid 0 and the adb channel reaching
the shell domain are the same operations either way — but the app-sandbox
starting point still needs a signed-APK run to confirm end to end.

### Coming back after a reboot

Two mechanisms, because one is not reliable enough.

`BOOT_COMPLETED` is the fast path: the agent is back about a second after boot
completes. But the broadcast is best-effort. Across four reboots of the same
build on the PA2353, one was dropped outright —

```
am_broadcast_discard_app: [0,…,BOOT_COMPLETED,187,ResolveInfo{dev.wanctl.agent/.BootReceiver}]
```

— while the same broadcast reached other apps in the same second. Android
discards a receiver whose process it cannot start, and a just-booted tablet was
running a load average of 30.

So the app also schedules a **persisted periodic job** (15 minutes, the shortest
JobScheduler allows) that starts the agent if it should be running and is not.
Worst case after a dropped broadcast the device is late by a quarter of an hour,
not absent until someone notices. Opening the app reconciles immediately.

### Why the binary is called `libwanctl.so`

Because that is the only way to get an executable file onto an Android device
that an app is allowed to run.

Android refuses `exec` of a file labelled `app_data_file` from the
`untrusted_app` domain, and every directory an app can write carries that label.
An APK's `lib/<abi>/` directory is labelled `apk_data_file` instead, which
`untrusted_app` *may* exec — but the package manager only extracts it onto disk
when the manifest says `android:extractNativeLibs="true"`, and it only extracts
files named `lib*.so`. So wanctl ships as `lib/arm64-v8a/libwanctl.so`, and on
the installed device it is:

```
/data/app/~~…/dev.wanctl.agent-…/lib/arm64/libwanctl.so
  -rwxr-xr-x system system u:object_r:apk_data_file:s0
```

Nothing `dlopen`s it. It is a program with a library's name. Termux ships its
own 24 MB bootstrap the same way.

The config, unlike the binary, lives in the app's private directory
(`files/wanctl`), which needs to be writable and does not need to be
executable.

### Updating

`wanctl update` does not work here and tells you so. The directory the app may
execute from belongs to the package manager and is read-only, so the unit of
update is the APK.

Tap **检查更新**. The binary fetches the signed release manifest, verifies its
Ed25519 signature and the APK's SHA-256 against the key compiled into it, and
hands the verified file to the system package installer, which then checks the
APK signature against the installed app's. Two independent signatures have to
agree before anything is replaced.

The first time, Android will ask you to allow wanctl to install apps.

## Termux

Still supported, still documented, no longer recommended. Everything below is
unchanged and was verified on 2026-08-06.

```sh
pkg install openssl-tool          # the installer verifies a signature with it
curl -fsSL https://relay.example.com/install.sh | sh
wanctl                            # Feishu login, then run detached
```

(The installer is at `/install.sh`, not `/dl/install.sh`. `/dl/` serves only what
the signed manifest names, so the URL this file carried until 2026-08-07 —
`/dl/install.sh` — returned 404 and the documented Termux one-liner had never
worked. Confirmed against the production relay.)

Termux costs four workarounds, all inside the binary, all consequences of one
rule: *anything the agent execs on Android must be named by its system absolute
path and must live outside an app's private data directory.* Termux itself only
runs its own binaries by preloading `libtermux-exec.so`, which rewrites every
`execve` to go through the dynamic linker — a CGO-free Go binary never loads it,
so it gets the interception's side effects without the interception's benefit:

- **argv[0] is duplicated.** The linker prepends the program's resolved path, so
  `wanctl version` arrives as `[wanctl, /abs/path/wanctl, version]` and every
  argument reads one slot late. The build detects and drops it. **v0.1.7 got
  this wrong** and a bare `wanctl` off `PATH` was unusable; fixed in v0.1.8.
- **`os.Executable()` returns the linker**, so `wanctl update` resolved its
  upgrade target to `/apex/com.android.runtime/bin/linker64`. On a rooted device
  that overwrites the system linker and takes the runtime down.
- **`wanctl start` cannot exec its own binary** and has to invoke the linker
  explicitly, the way Termux does.
- **`getprop` off `PATH` finds Termux's copy**, which is equally unexecutable,
  so device names silently fell back to `wanctl-agent`. The absolute path
  `/system/bin/getprop` is used instead.

None of these exist in the APK, because nothing intercepts its execs.

`wanctl service install` refuses on Android: the platform gives an unprivileged
process no service manager to install into. Under Termux the nearest equivalents
are `termux-wake-lock`, the separate **Termux:Boot** app, and
`pkg install termux-services`. **None of the three has been verified by wanctl.**

## Without installing anything (adb)

Useful for a device already on USB.

```sh
adb push wanctl-android-arm64 /data/local/tmp/wanctl
adb shell chmod 755 /data/local/tmp/wanctl
adb shell /data/local/tmp/wanctl        # login + detached agent
```

`/data/local/tmp` is the one place the adb shell user can both write and
execute. Note that it is shared with every other app and with anyone else who
has adb — the APK's private directory is not.

## What is different on Android, and why

**DNS.** Android has no `/etc/resolv.conf` — resolution goes through `netd`,
reachable only via bionic's libc. wanctl ships CGO-free static binaries, so the
Go resolver would fall back to `127.0.0.1:53` and every lookup would fail with
`connection refused` before a single packet reached the relay. The Android build
points the resolver at explicit nameservers instead (AliDNS, Google, Cloudflare,
rotating so a retry reaches a different operator). Full reasoning in
`docs/adr/0002-android-resolver.md`.

Override it when the relay is on a private or split-horizon zone:

```sh
export WANCTL_DNS=10.0.0.53,10.0.0.54     # comma-separated, :53 assumed
```

A device that does have `/etc/resolv.conf` (a proot distro, a rooted setup) is
left alone. Termux's own `$PREFIX/etc/resolv.conf` is read when present.

**Config directory.** The app passes `WANCTL_CONFIG_DIR` explicitly. Elsewhere:
in Termux `$HOME` is real and the config lands in `$HOME/.config/wanctl`; under
an adb shell `HOME=/` is read-only, so the agent falls back to `$TMPDIR/wanctl`,
then `.wanctl` next to the binary, then `/data/local/tmp/.wanctl`.

**Shell.** Sessions run in `/system/bin/sh` (mksh), the one shell present on
every Android build. `/bin/sh` exists only from Android 11 on. Termux's own
`$PREFIX/bin/sh` is deliberately not used even in Termux — the agent physically
cannot start it. You lose nothing: a `/system/bin/sh` session inherits Termux's
`PATH`, and Termux binaries started *by that shell* run normally, because mksh
performs those execs. Verified over the relay: `git --version` → 2.52.0,
`python --version` → 3.12.12, both Termux's.

**File transfers.** Android grants the shell user traverse-only access to the
directories leading anywhere useful (`/data` is `drwxrwx--x system:system`), and
`os.Root` must *open* each component on the way down. Binding a transfer to the
volume root therefore failed with `openat data/local/tmp: permission denied`
even against a writable destination. On Android an otherwise-unconstrained
transfer binds to the target's own directory instead — narrower than the volume
root, not wider, and symlinks at the destination are still refused.

**Device name.** Every Android device reports its hostname as `localhost`, so
the agent asks the property service instead (`ro.product.marketname`, then
`ro.product.model`) via `/system/bin/getprop`, and registers as e.g. `pa2353`.

## Building the APK

```sh
./scripts/build-apk.sh              # dev build, debug-signed
./scripts/build-apk.sh v0.1.11      # release build; needs the keystore env
```

It needs the Android SDK (`ANDROID_HOME`, or `~/Library/Android/sdk`) and a JDK
(Android Studio's bundled one is found automatically). There is no Gradle and no
AndroidX; the chain is `aapt2 → javac → d8 → zipalign → apksigner`, so the build
needs no network. The trade-off — you cannot open `android/` in Android Studio —
is argued in `docs/adr/0003-android-apk.md`.

Release signing reads `WANCTL_ANDROID_KEYSTORE` (or `..._B64`),
`WANCTL_ANDROID_KEYSTORE_PASS`, and `WANCTL_ANDROID_KEY_ALIAS`. Without them it
falls back to the debug key and says so — such an APK installs on the developer's
own device and is worthless to anyone else. **Losing the release keystore means
every installed device must uninstall and reinstall**; Android has no recovery
path for a changed signing key.

`scripts/build-release.sh` picks up an APK staged at
`build/android/wanctl-android-arm64.apk`, or builds one, and refuses to cut a
release without it unless `WANCTL_SKIP_APK=1` says so — a manifest with no APK
entry strands every installed app on its current version.

## When the device is "running" but the controller cannot see it

The app showing ● 运行中 means the agent process is alive. Whether the *relay*
still lists the device is a separate fact, and the two can disagree: the agent
prints "online via …" once when it starts and never contradicts it, while its
registration is kept alive by a poll loop that can fail silently.

Since 2026-08-07 the poll loop says so. Check 查看日志 (or `adb logcat -s wanctl:I`)
for:

```
wanctl: relay poll failed: … write: software caused connection abort (1 consecutive)
```

A handful of failures around a Wi-Fi change is normal and self-correcting — that
example recovered on the next attempt. A count that keeps climbing means the
device genuinely cannot reach the relay, and the next thing to check is whether
the network blocks the public DNS resolvers the Android build uses (see ADR 0002
and `WANCTL_DNS`), because `ping` and the browser will keep working while wanctl
does not.

## Testing on a device

- **`adb install --no-incremental`.** The default incremental install leaves the
  APK on `incremental-fs`; the moment adb disconnects, every read inside it
  fails with `ETIME` (`Timer expired`, exec exits 126) — which looks exactly
  like an SELinux exec denial and is not one.
- **`run-as` is not the app's domain.** It runs as `runas_app`, not
  `untrusted_app`. Useful as a probe, worthless as proof.
- **A release-signed APK is not debuggable**, so `run-as` and
  `adb shell cat files/…` do not work on it. Read `adb logcat -s wanctl:I`, or
  the app's own 查看日志 screen.

## Verified, and on what

On a vivo PA2353 (Android 13, arm64) on 2026-08-07, against the **production**
relay, driving it from a Mac — installed APK, app UI, no adb shortcuts on the
wanctl side:

- the binary execs from `nativeLibraryDir` **in the `untrusted_app` domain**
  (`id -Z` → `u:r:untrusted_app:s0:c10,c257,c512,c768`), which `run-as` cannot
  demonstrate because it runs as `runas_app`
- enrollment through the app's own 登录 button (`login --code`), token stored,
  device registered as `pa2353`
- `exec` with streamed output and a real exit code (42 out, 42 back)
- 512 KB `push` and `pull`, byte-identical round trip
- **reboot ×2, screen asleep, nobody touching the device → controllable from the
  Mac** — the thing the Termux route never did
- `update --fetch-apk` downloading and verifying a signed APK against a relay,
  hash identical to the built artifact, and the up-to-date branch returning
  empty stdout with exit 0

And on 2026-08-07, once v0.1.11 was deployed and `/dl` served an APK for the
first time, **the whole in-app update**: 检查更新 → PackageInstaller → running
the new build, from the app's own UI. Checked afterwards from the controller
rather than taken on the app's word:

- the installed `base.apk` hashes to the published artifact byte for byte
  (`sha256sum $(pm path dev.wanctl.agent)` against `/dl`'s manifest entry)
- the binary the app execs reports `v0.1.11`, still under `id -Z` →
  `u:r:untrusted_app:s0`
- **the device's identity fingerprint is unchanged.** That is the part worth
  keeping: the key lives in the app's private data, which only survives an
  in-place upgrade. A signature mismatch would have forced an uninstall and
  produced a new fingerprint, so this is what proves the two signature chains —
  the manifest's Ed25519 and the APK's certificate — agreed independently.

## Not yet verified

- **Any device that is not a vivo PA2353.** The OEM gates (autostart lists,
  background-power managers) differ per vendor and are the most likely reason
  for "it stopped coming back after reboots" on a device nobody has tried.
- **Termux:Boot / termux-services** — the documented Termux mechanisms for
  surviving a reboot, not a tested wanctl integration.
