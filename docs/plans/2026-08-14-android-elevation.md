# Android: shell-UID elevation and the adb command surface

**Goal:** make `wanctl exec` against an Android phone able to do everything
`adb shell` can do on a developer-mode device — `pm`, `am`, `input`,
`screencap`, `dumpsys`, `settings`, `wm`, `svc`, `logcat` — instead of the
handful of things an app sandbox permits.

## The wall this is about

The APK agent runs in the strictest domain Android has for third-party code.
Measured on the target device (`zyldephone`, OPPO PGBM10, Android 14 / SDK 34)
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

## Three channels, probed in order

Decided by the owner on 2026-08-14: build all three and degrade at runtime.

| channel | how it gets uid 2000/0 | needs | cost to the user |
|---|---|---|---|
| `su` | `su -c` | rooted device | none, if already rooted |
| `shizuku` | Java binder → `IShizukuService.newProcess()` | Shizuku installed **and started** | must install Shizuku; must restart it after each reboot |
| `adb` | in-process adb client → `127.0.0.1:<adbd port>` | Developer options → Wireless debugging | pairing code once; re-enable wireless debugging after each reboot |
| `none` | current app-sandbox shell | — | (fallback, not an elevation) |

Probe order is `su` → `shizuku` → `adb` → `none`; the first available one wins.
A controller may pin one with `--via`, and pinning an unavailable channel is an
error rather than a silent downgrade — a command that quietly ran unprivileged
after you asked for root is a wrong answer dressed as a right one.

**None of the three is free of a reboot story, and the docs must say so.**
`su` survives reboots. Shizuku and wireless debugging both do not: Android
clears wireless debugging on boot, and Shizuku's own service dies with it. That
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

**Phase 3 — the Shizuku channel.**
Done when: on PGBM10 with Shizuku running, `--via shizuku -- id` returns
`uid=2000` and the probe picks it with no `--via`.

**Phase 4 — the adb self-connect channel.**
Done when: on PGBM10 with only *Developer options → Wireless debugging* on and
a pairing code entered in the wanctl app, `--via adb -- id` returns `uid=2000`,
and it still connects after the app is restarted without re-pairing.

## Verification devices

- `zyldephone` — OPPO PGBM10, Android 14, **not rooted**. The target for
  phases 3 and 4. Currently online, agent healthy.
- A rooted device is needed for phase 1's acceptance (`su`). The Xiaomi Mi 10
  Ultra rooted on 2026-08-06 is the candidate; it does not yet run wanctl.

## Status as of 2026-08-14 10:30 (handover)

Branch `feat/android-elevation`, MR !54. `go test ./...` green; APK builds.

**Done and verified on real hardware**

- Phases 1 and 2 complete. On a Xiaomi Mi 10 Ultra (Android 11, Magisk) over a
  live relay: plain exec `uid=2000`, `--elevate` `uid=0 u:r:magisk:s0`,
  `screenshot` returns a real PNG, verbs work, event log records the channel.
- The adb wire protocol works end to end against a real adbd (android-29
  emulator): banner/feature parsing, `id` as uid 2000, exit code 42 intact.
- **SPAKE2 pairing interoperates with AOSP.** On the OPPO Reno8 (PGBM10,
  Android 14, NOT rooted), `adb-pair 43631 046087` succeeded and the device's
  own 无线调试 → 已配对的设备 list now shows `wanctl@pgbmtest`. The device UI
  confirming it is the strongest evidence available.
- On that same phone the adb channel now completes TLS: the paired key is
  accepted, CNXN succeeds, and the full device banner comes back.

**The one thing that does not work yet**

On the Reno8, after a successful TLS connect and CNXN, `A_OPEN` for a shell
service gets no reply at all and the command times out. The same code works
against the emulator's adbd over plaintext. Ruled out, each by measurement:

| hypothesis | result |
|---|---|
| header+payload split across two writes (two TLS records) | changed to one write; no effect (kept anyway — it is better) |
| wrong TLS client certificate | fixed and confirmed: Go silently drops `Certificates` that do not match the server's CertificateRequest; `GetClientCertificate` was required. TLS now succeeds |
| `delayed_ack` window in `A_OPEN.arg1` (device advertises the feature) | tried 32 MB; no effect; **reverted** rather than leave a guess in the code |
| pairing not actually registered | disproved — the device lists the key |
| a stale/wrong port | disproved — the banner comes back from 37819 |

Not yet tried: comparing our byte stream against the real `adb` client's with a
capture on the loopback interface. That is the obvious next move and would
settle it — every remaining hypothesis is about what AOSP sends that we do not.

Note the port had to be given by hand (`WANCTL_ADB_PORT=37819`, read off the
device's wireless-debugging screen). mDNS discovery — the Java `NsdManager`
side that fills the state file — is not built yet.

## Open items

- Whether an OEM ROM (ColorOS 14 here) permits an app to reach loopback adbd at
  all is unverified. Shizuku and LADB do this on stock and on several OEM ROMs,
  but this specific ROM has not been tried; phase 4 starts with that probe.
- Google has an open proposal to restrict local (on-device) adb connections. It
  is a comment on an open issue with no implementation date as of 2026-08-14 —
  worth knowing, not worth designing around, and an argument for keeping `su`
  and Shizuku as peers of the adb channel rather than fallbacks beneath it.
