# ADR 0004 — Android elevation is three interchangeable channels, and the adb one is written in Go

Status: accepted (2026-08-14)

Builds on ADR 0003, which established that the APK agent runs — deliberately —
in `untrusted_app`. This ADR is about what to do when that is not enough.

## Context

`wanctl exec` against an Android device can do what an app can do, which is a
small fraction of what `adb shell` can do. The gap is not a missing feature;
it is a uid and an SELinux domain. `adb shell` is uid 2000 in domain `shell`;
the agent is uid 10601 in `untrusted_app`. Everything the owner asked for —
`pm`, `am`, `input`, `screencap`, `dumpsys`, `settings`, `wm`, `svc` — is on
the other side of that line. Measured on PGBM10 / Android 14 on 2026-08-14,
even `settings get global adb_wifi_enabled` throws for the app UID.

v0.1.12 answered one instance of this (battery) by collecting the value in Java
and handing it to the Go child through a file. That was right for one value and
is wrong as a strategy: most of adb's surface has no app-visible equivalent at
any price, so per-verb workarounds asymptote well short of the goal.

Three mechanisms exist that actually cross the line, and the owner chose to
build all three (2026-08-14): `su` on a rooted device, Shizuku's binder, and an
adb client connecting to the device's own adbd over loopback. They are peers,
not a preference chain with a "real" answer at the bottom — each is unavailable
on some devices, and the local-adb one additionally has a public proposal at
Google to restrict it (an open issue with no implementation date as of this
writing). Anything that hard-wires one of them would have to be rewritten when
that landscape moves.

## Decision

**One `elevate.Channel` interface with four implementations** (`su`, `shizuku`,
`adb`, and `none` for the current sandbox shell), probed in that order, with
`--via` to pin one. Pinning an unavailable channel is an error; it never
silently degrades, because a command that ran unprivileged after the caller
asked for root is a wrong answer wearing a right answer's clothes.

**Elevated commands are a separate policy class** (`KindExecElevated`), and
`bypass` mode does not cover it. The existing "auto-allow everything" switch
exists so an unattended device is usable at all; it must not also be the thing
that hands out uid 0 without anyone deciding to.

**The adb channel is implemented in Go, in-process.** Not by shipping a real
`adb` binary in the APK, which is what LADB does and what `lib/arm64-v8a/`
would happily host.

## Why not ship the adb binary

The only prebuilt Android/arm64 adb in existence is Termux's. Inspected on
2026-08-14 (`android-tools_36.0.1+really35.0.2_aarch64.deb`):

```
NEEDED:  liblog.so libc.so libprotobuf.so libz.so.1 libbrotlidec.so
         libbrotlienc.so liblz4.so libzstd.so.1 libc++_shared.so libdl.so
RUNPATH: /data/data/com.termux/files/usr/lib
```

Three things follow, and the third is the one that decides it:

1. **Seven libraries have to come along**, ~8 MB, for a feature that is one of
   three channels and unavailable on many devices.
2. **Two of them cannot come along as they are.** The package manager extracts
   only `lib*.so` from `lib/<abi>/` (ADR 0003), and `libz.so.1` / `libzstd.so.1`
   do not match. Shipping them means renaming files and rewriting `DT_NEEDED`
   with patchelf at build time — a new build-time dependency, on a tool that
   edits ELF headers, in a chain whose current virtue is that it is `aapt2 →
   javac → d8 → zipalign → apksigner` and nothing else.
3. **It would put a third-party prebuilt binary inside our signed APK.** This
   app's update story is two independent signature chains agreeing (ADR 0003,
   and verified in production on 2026-08-07). Downloading a Debian package from
   packages.termux.dev at build time contradicts that, and contradicts
   `build-apk.sh`'s network-free design — which is not fastidiousness: this
   project's CI runner cannot reach proxy.golang.org, which is precisely why the
   build has no resolver in it today.

Vendoring the binary into the repo instead of fetching it removes the network
problem and keeps the other two, including the part where we would be signing
and shipping 2.8 MB of someone else's build with no way to reproduce it.

## What the Go implementation costs

The adb wire protocol (`A_CNXN`/`A_AUTH`/`A_OPEN`/`A_WRTE`/`A_OKAY`/`A_CLSE`)
is documented in AOSP and stable for a decade; RSA-2048 token signing is
standard-library work. The genuinely hard part is Android 11+ pairing: TLS plus
**BoringSSL's SPAKE2 over Ed25519**, which is *not* RFC 9382 and *not* what
`gospake2` (python-spake2-compatible) implements — checked on 2026-08-14, no Go
library implements adb's variant. It has to be written against BoringSSL's
`spake25519.c`.

That is the real price of this decision, and it is accepted for one reason: the
failure mode is binary and cheap to test. Pairing either completes or it does
not, on a device we have, in one round trip. Compare it with the alternative's
failure mode — a patched third-party ELF that loads on the test device and not
on someone else's ROM — which is neither binary nor cheap.

The port adbd listens on is discovered by the Java side (`NsdManager`,
`_adb-tls-connect._tcp`) and handed to the Go child through the same state-file
mechanism v0.1.12 built for battery, because an app needs a `MulticastLock` to
receive mDNS and that is a framework call.

## Consequences

- Four channel implementations to maintain, and their *differences* are the
  documentation burden: `su` survives reboots, Shizuku and wireless debugging
  do not. A device that must be reachable unattended after a power cut should
  be rooted or should not depend on elevation. Said plainly in `docs/android.md`
  rather than discovered.
- If Google restricts local adb, the adb channel degrades and the other two are
  untouched. That is the main return on building three.
- The SPAKE2 implementation is security-relevant code we own. It is confined to
  `internal/adb`, is used for nothing else, and a bug in it fails closed
  (pairing does not complete) rather than opening a hole — the resulting
  connection is still TLS to loopback with a key the device recorded.
- Elevation being a separate policy class means an existing bypass-mode device
  gains nothing on upgrade. Deliberate: no device becomes more powerful because
  someone updated it.
