# Android: shell-UID elevation and the adb command surface

**Goal:** make `wanctl exec` against an Android phone able to do everything
`adb shell` can do on a developer-mode device — `pm`, `am`, `input`,
`screencap`, `dumpsys`, `settings`, `wm`, `svc`, `logcat` — instead of the
handful of things an app sandbox permits.

## The wall this is about

The APK agent runs in the strictest domain Android has for third-party code.
Measured on the target device (`my-phone`, OPPO PGBM10, Android 14 / SDK 34)
on 2026-08-14:

```
uid=10601(u0_a601) … context=u:r:untrusted_app:s0:c89,c258,c512,c768
$ settings get global adb_wifi_enabled
Exception occurred while executing 'get':      ← app UID may not call it
$ getprop init.svc.adbd
stopped
```

`adb shell` runs as uid 2000 (`shell`), in the `shell` SELinux domain, and is a
member of groups an app never joins. That difference — not any missing wanctl
feature — is why `dumpsys battery` was unreachable and why v0.1.12 had to
collect battery state through a Java `BroadcastReceiver` instead. Adding more
verbs that way does not scale to "all of adb": most of adb's surface is
`shell`-domain binaries and framework calls guarded by `android.permission`
signature checks, with no app-visible equivalent.

So the unit of work is not "more verbs". It is **one elevation channel**, after
which the entire adb surface follows for free.

## The channels, probed in order

Decided by the owner on 2026-08-14: build them all and degrade at runtime. Three
were planned; Shizuku was cut the same day, once the adb channel worked.

| channel | how it gets uid 2000/0 | needs | cost to the user |
|---|---|---|---|
| `su` | `su -c` | rooted device | none, if already rooted |
| `adb` | in-process adb client → `127.0.0.1:<adbd port>` | Developer options → Wireless debugging | pairing code once; re-enable wireless debugging after each reboot |
| `none` | current app-sandbox shell | — | (fallback, not an elevation) |

Probe order is `su` → `adb` → `none`; the first available one wins. A controller may pin one with `--via`, and pinning an unavailable channel is an
error rather than a silent downgrade — a command that quietly ran unprivileged
after you asked for root is a wrong answer dressed as a right one.

**Neither is free of a reboot story, and the docs must say so.**
`su` survives reboots; wireless debugging does not — Android clears it on boot. That
is a platform fact, not something wanctl can engineer around, and a device that
must be reachable unattended after a power cut should be rooted or should not
depend on elevation.

## Why the adb channel is written in Go rather than shipped as a binary

The tempting shortcut is LADB's: put a real `adb` executable in the APK.
`lib/arm64-v8a/` is already the one place this app may exec from — that is how
`libwanctl.so` works — so it looks like a drop-in.

It is not. Checked against `android-tools_36.0.1+really35.0.2_aarch64.deb`
(Termux, the only prebuilt Android/arm64 adb that exists) on 2026-08-14:

```
NEEDED:  liblog.so libc.so libprotobuf.so libz.so.1 libbrotlidec.so
         libbrotlienc.so liblz4.so libzstd.so.1 libc++_shared.so libdl.so
RUNPATH: /data/data/com.termux/files/usr/lib
```

Seven of those must be carried along, and two of them — `libz.so.1`,
`libzstd.so.1` — cannot be: the package manager extracts only files matching
`lib*.so`, and neither name ends in `.so`. Shipping them means renaming the
files and rewriting `DT_NEEDED` with patchelf at build time, adding ~8 MB, and
pulling a third-party prebuilt binary into an APK whose whole update story rests
on two independent signature chains agreeing. `scripts/build-apk.sh` is
deliberately network-free (this project's CI runner cannot even reach
proxy.golang.org); `curl`-ing a Debian package into the build would be the
flakiest link in the pipeline as well as the least trustworthy one.

The Go implementation is a known quantity by comparison: the adb wire protocol
is stable and documented in AOSP (`OVERVIEW.TXT`, `protocol.txt`), and the one
genuinely hard part is pairing. See ADR 0004.

## Policy: elevated commands are their own class

Also decided by the owner: elevation is off by default and gated separately.

- A new `policy.KindExecElevated`. Rules for it are stored, matched and
  approved independently of `KindExec`.
- **`bypass` mode does not cover it.** The 自动放行所有命令 switch exists so an
  unattended device can work at all; it must not silently also hand out uid 0.
  An elevated command on a bypass-mode device still needs an explicit rule or a
  human approval in the portal.
- The APK gets its own switch (default off). With it off, the channel is not
  probed and `--elevate` fails with a message naming the switch.
- Every elevated command is written to the event log with the channel that ran
  it, so `wanctl logs` can answer "what ran as root on this phone".

## Command surface

Two layers, per the owner's decision.

**Pass-through** — the whole adb surface, no wrapping:

```sh
wanctl exec --target phone --elevate -- pm list packages -3
wanctl exec --target phone --elevate --via su -- dumpsys battery
```

**Structured verbs** — the high-frequency things, made pleasant. They run
through the same elevation channel and the same policy class, but handle the
parts that are awkward over a text pipe:

| verb | wraps | why it needs wrapping |
|---|---|---|
| `screenshot [-o f]` | `screencap -p` | binary stdout; lands as a local file |
| `screenrecord [-t n]` | `screenrecord` | binary, and needs a stop signal |
| `input tap/swipe/text/key` | `input` | argument shape; text needs quoting care |
| `app list/info/install/uninstall/start/stop/clear` | `pm`, `am` | five different tools, one noun |
| `settings get/put` | `settings` | namespace argument is easy to get wrong |
| `logcat [-f]` | `logcat` | streaming; must not buffer to completion |
| `prop get/set` | `getprop`/`setprop` | — |
| `battery`, `wifi`, `bt`, `ps`, `meminfo` | `dumpsys` | parsed to JSON, stable shape |

`battery` already exists as a sandbox verb and keeps working with no elevation;
where a verb has both a sandbox and an elevated implementation, the sandbox one
is used unless it cannot answer.

## Delivery, in order, each independently verifiable

**Phase 1 — the channel abstraction, `su`, policy, `--elevate`.**
Done when: on a rooted device, `wanctl exec --target <dev> --elevate -- id`
returns `uid=0`; the same command on a bypass-mode device with no elevated rule
is refused; the event log names the channel.

**Phase 2 — structured verbs.**
Done when: `wanctl exec --target <dev> --elevate -- screenshot -o /tmp/s.png`
writes a PNG that opens, and `logcat -f` streams rather than buffering.

**Phase 3 — the Shizuku channel. Cut (owner's decision, 2026-08-14).**
Once phase 4 worked, Shizuku had no scenario left to itself: it lands on the
same uid 2000, it is *started* by wireless debugging (so it needs everything the
adb channel needs and then a second app on top), and it dies with each reboot
just the same. `--via shizuku` reports that it was dropped rather than that it
is unknown, because the name is in earlier docs. See ADR 0004's amendment.

**Phase 4 — the adb self-connect channel.**
Done when: on PGBM10 with only *Developer options → Wireless debugging* on and
a pairing code entered in the wanctl app, `--via adb -- id` returns `uid=2000`,
and it still connects after the app is restarted without re-pairing.

## Verification devices

- `my-phone` — OPPO PGBM10, Android 14, **not rooted**. The target for
  phase 4. Currently online, agent healthy.
- A rooted device is needed for phase 1's acceptance (`su`). The Xiaomi Mi 10
  Ultra rooted on 2026-08-06 is the candidate; it does not yet run wanctl.

## Status as of 2026-08-14 11:10

Branch `feat/android-elevation`, PR #54. `go test ./...` green; APK builds.

**Done and verified on real hardware**

- Phases 1 and 2 complete. On a Xiaomi Mi 10 Ultra (Android 11, Magisk) over a
  live relay: plain exec `uid=2000`, `--elevate` `uid=0 u:r:magisk:s0`,
  `screenshot` returns a real PNG, verbs work, event log records the channel.
- The adb wire protocol works end to end against a real adbd (android-29
  emulator): banner/feature parsing, `id` as uid 2000, exit code 42 intact.
- **SPAKE2 pairing interoperates with AOSP.** On the OPPO Reno8 (PGBM10,
  Android 14, NOT rooted), `adb-pair` succeeded twice with codes typed off the
  device's own screen, and 无线调试 → 已配对的设备 lists `wanctl@pgbmtest`.
- **Phase 4's protocol half is done.** On that same Reno8, over the wireless
  debugging port with no root and no cable:
  - `--elevate --via adb -- id` returns in 0.3s where it used to time out;
  - the shell it opens is a child of `/apex/com.android.adbd/bin/adbd`, not of
    the agent — checked by reading `/proc/$PPID/cmdline` through both channels,
    which is what separates "the adb channel worked" from "the sandbox answered";
  - `pm list users` works, exit code 42 survives the round trip,
    `wanctl screenshot -via adb` returns a real 1080×2400 PNG;
  - the device's own 无线调试 screen shows `wanctl@pgbmtest — 当前已连接`;
  - it reconnects after the agent restarts, with no re-pairing;
  - the event log records `"via":"adb"` per command.

**The A_OPEN silence: root cause found**

A client must **not** send `A_CNXN` again after the TLS handshake. adbd brings
the transport online and sends its banner itself the moment the handshake
succeeds (`daemon/adb_wifi.cpp`, `adbd_wifi_secure_connect` → `handle_online()`
+ `send_connect()`). A second CNXN is read as a brand-new connection:
`handle_new_connection()` opens with `handle_offline()`, and for a `use_tls`
transport it replies with another STLS request instead of coming back online.
Then `adb.cpp`'s `A_OPEN` case begins

```c
if (!t->online || p->msg.arg0 == 0) break;   // no CLSE, no log line
```

so every stream request is discarded in silence while the connection still looks
healthy — banner parsed, features negotiated, socket open.

The evidence, in the order it landed: `adb logcat` showed `host-32: offline` and
`ADB wifi device disconnected` **one millisecond after**
`adbd_wifi_secure_connect: connected host-32`, which is a transport going
offline right after coming online; the android14-release source then named the
exact `break` that eats the packet.

That also retires the `delayed_ack` hypothesis for good, and it is worth
knowing why: the mismatch branch is the *next* one down, it calls
`send_close()` and it `LOG(ERROR)`s. A delayed-ack mismatch would have produced
a CLSE and a logcat line. We had neither.

Regression test: `TestShellOverTLSSendsNoSecondCnxn`, against a fake adbd that
models the online/offline rule rather than asserting on bytes. It was confirmed
to fail — with the right message — when the extra CNXN is put back.

**Port discovery is built.** `AdbPortWatcher` watches mDNS for
`_adb-tls-connect._tcp` through `NsdManager`, keeps a `MulticastLock` while it
does, ignores every advertisement whose address this device does not hold — a
Wi-Fi full of phones with wireless debugging on all publish that service — and
writes the port into the same state file the battery verb uses.
`WANCTL_ADB_PORT` still overrides it, which is what the Termux and adb-shell
routes need.

**Phase 4 closed in the APK, in `untrusted_app`.** A test build (package
`dev.wanctl.agent.test`, so the production agent was never touched) on the same
Reno8, with 提权通道 on and no `WANCTL_ADB_PORT` anywhere:

```
exec            → uid=10606(u0_a606) … context=u:r:untrusted_app:s0:c94,…
exec --elevate  → uid=2000(shell)    … context=u:r:shell:s0
```

Same agent process, same command, two channels. The app's state file carried
`"adb":{"port":41031}` — the number on the device's own wireless-debugging
screen — beside the battery fields. `pm list users`, `screenshot` (a real
1080×2400 PNG) and the no-`--via` probe all work from there, and the event log
records `"via":"adb"`.

That answers the open item this plan has carried since it was written: **ColorOS
14 does let an app uid reach its own adbd over loopback.**

## Open items

- `AdbPortWatcher.onServiceLost` — the path that clears a port when wireless
  debugging goes off — is **not** exercised yet. Toggling wireless debugging
  needs the Settings UI on this ROM (`settings put global adb_wifi_enabled`
  is refused for uid 2000 on ColorOS: `must have one of:
  [android.permission.WRITE_SECURE_SETTINGS]`, and the wireless-debugging
  activity is not at its AOSP name). Worst case if it is wrong: a stale port
  that the Go side ages out after 30 minutes and reports as unavailable.
- Google has an open proposal to restrict local (on-device) adb connections. It
  is a comment on an open issue with no implementation date as of 2026-08-14 —
  worth knowing, not worth designing around, and an argument for keeping `su`
  and Shizuku as peers of the adb channel rather than fallbacks beneath it.
