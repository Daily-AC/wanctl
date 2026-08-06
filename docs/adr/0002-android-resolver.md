# ADR 0002 — The Android build carries its own DNS resolver configuration

Status: accepted (2026-08-05)

## Context

wanctl ships `CGO_ENABLED=0` static binaries on every platform. That choice is
what makes the release pipeline a plain `go build` per target and what makes the
binaries drop onto arbitrary machines without a libc to match.

Android breaks it. There is no `/etc/resolv.conf` on Android: name resolution
goes through `netd`, which is reachable only through bionic's libc — i.e. only
from a cgo build. With CGO off, Go's pure resolver finds no configuration and
falls back to its compiled-in default of `127.0.0.1:53`, where nothing is
listening. Measured on a vivo PA2353 (Android 13) and an Android 16 emulator on
2026-08-05:

```
lookup wanctl-relay.***REMOVED***.***REMOVED***.com on [::1]:53: read udp [::1]:38720->[::1]:53:
read: connection refused
```

Every dial fails, so an Android device cannot reach the relay at all — the agent
is not degraded, it is entirely non-functional. `getprop net.dns1`, the classic
workaround, has been empty since netd stopped publishing it around Android 8;
it read empty on both test devices.

## Decision

The Android build installs a process-wide `net.Resolver` (`internal/androiddns`)
pointed at explicit nameservers, chosen in this order:

1. `WANCTL_DNS` — an operator override, for private or split-horizon zones.
2. `/etc/resolv.conf`, if the device has one — then the stock resolver is
   already correct and we change nothing.
3. `$PREFIX/etc/resolv.conf` — Termux's own copy, which Go never looks at.
4. Built-in public resolvers: AliDNS, Google, Cloudflare, rotated per dial so
   Go's own query retries land on a different operator.

This is Android-only, behind `//go:build android`, and compiles to nothing
elsewhere.

## Alternatives rejected

**An NDK/cgo Android build.** The correct answer in the abstract: cgo gets
resolution through bionic and therefore honours whatever the device is
configured with, including a VPN's DNS. Rejected because it costs a C
cross-compiler in the release pipeline and gives up static linking — for one
platform out of six. The whole release story (build, sign, verify, `/dl`) is
uniform today, and this would make Android the one target that needs a toolchain
nobody else needs.

**DNS-over-HTTPS against an IP literal.** Verified working on the device, and it
needs no prior DNS. Rejected as the default because it puts an HTTP client
underneath the network stack that the HTTP client itself sits on, and because it
is more conspicuous on the wire than UDP/53 in exactly the networks where wanctl
is used.

**Refusing to run on Android without `WANCTL_DNS`.** Honest but useless: the
common case is a phone on Wi-Fi or mobile data where public resolvers work fine,
and demanding configuration for it would make the first-run experience a failure
message.

## Consequences

The uncomfortable one: on Android, wanctl does **not** use the device's
configured DNS. A device on a VPN whose private zones are served by the VPN's
resolver will not resolve those names — `WANCTL_DNS` is the escape hatch, and
`docs/android.md` says so. This is a real regression against what an Android app
would do, accepted because the alternative is a binary that resolves nothing at
all.

Public-resolver reachability is now part of wanctl's reachability on Android. A
network that blocks outbound 53 to all three operators takes the agent down even
though the relay is reachable. If that shows up in practice, DoH against an IP
literal is the fallback to reach for — it was measured working and is a
contained change to `internal/androiddns`.
