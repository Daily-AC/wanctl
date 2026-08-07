# ADR 0003 — Android ships as an APK, and the APK is built without Gradle

Status: accepted (2026-08-06)

Supersedes the Termux-hosted arrangement described in ADR 0002's context, though
not ADR 0002 itself: the Android build still carries its own DNS resolver, and
that is still necessary inside an app sandbox (measured again on 2026-08-06 —
the agent reached the production relay from the app's UID and got a clean 401
for a bogus token).

## Context

Two Android rules, neither negotiable, produced everything that follows:

1. **Go's Android target is always dynamic PIE.** `-buildmode=exe` does not even
   compile there, so every wanctl binary names `/system/bin/linker64` as its ELF
   interpreter.
2. **`untrusted_app` may not exec a file labelled `app_data_file`** — which is
   every directory an app can write, including everything under Termux's
   `$PREFIX`.

Termux works around (2) by preloading `libtermux-exec.so`, which rewrites every
`execve` to invoke the dynamic linker explicitly. A CGO-free Go binary loads no
such library, so wanctl gets neither the workaround nor an exemption — and it
does get the interceptor's side effect, because Termux's shell applies it to
*wanctl's* children. The result was four separate defects, fixed across v0.1.8
and v0.1.9:

- the linker prepends the program path to `argv`, so every subcommand read one
  slot late and a bare `wanctl` was inert;
- `os.Executable()` answers `linker64`, so `wanctl update` resolved its own
  binary to the *system dynamic linker* — on a rooted device that overwrites it
  and takes the Android runtime down;
- `wanctl start` could not exec its own binary and had to imitate Termux by
  invoking the linker itself;
- `getprop` resolved via `PATH` found Termux's copy, which is equally
  unexecutable, so device names silently fell back to `wanctl-agent`.

Every one of those depends on Termux implementation details that a Termux
release can change. And the arrangement still did not survive a reboot, which is
the property an always-on agent most needs.

## Decision

**Ship the Android agent as an APK that carries the binary as a native library.**

An APK's `lib/<abi>/` directory is labelled `apk_data_file`, which
`untrusted_app` *is* permitted to exec. Measured on a vivo PA2353 (Android 13)
on 2026-08-06, on an APK built and signed by this repo:

```
/data/app/~~…/com.***REMOVED***.wanctl-…/lib/arm64/libwanctl.so
  -rwxr-xr-x system system u:object_r:apk_data_file:s0
```

The binary is named `libwanctl.so` and declared with
`android:extractNativeLibs="true"`. It is not a shared library and nothing
`dlopen`s it; the name is what makes the package manager extract it onto disk
and set the executable bit. Termux ships its own 24 MB bootstrap the same way,
so this is the platform's established shape for the problem, not a trick.

All four defects above cease to exist rather than being fixed: nothing
intercepts exec, so `argv` arrives intact, `os.Executable()` is correct, the
binary can exec itself, and `/system/bin/getprop` is reached by absolute path.

A foreground service supervises the agent, and a `BOOT_COMPLETED` receiver
restarts it — the reboot problem Termux never solved.

**The APK is built with `aapt2 → javac → d8 → zipalign → apksigner`, driven by
`scripts/build-apk.sh`. No Gradle, no AndroidX, no Kotlin.**

## Alternatives rejected

**Keep the Termux route.** It works today and is not being deleted — the
`android/arm64` binary still ships and `docs/android.md` still documents it, for
people who already live in Termux. It is no longer the recommended path because
its four workarounds are load-bearing on another project's internals, and
because it cannot survive a reboot.

**Gradle.** The conventional choice, and the reason to reject it is specific to
this repository rather than a general opinion about build systems. This
project's CI already runs jobs on a runner that cannot reach
`proxy.golang.org` — hence the `GOPROXY` and `TRIVY_DB_REPOSITORY` overrides in
`.gitlab-ci.yml`. A build system whose first act is to resolve a dependency
graph from Maven Central would put the Android artifact behind the least
reliable link in the pipeline, and the app it would be resolving dependencies
for is a few hundred lines of framework Java with no third-party code at all.
The SDK's own command-line tools need no network.

The cost is real and worth stating: **you cannot open `android/` in Android
Studio as a project.** Editing is a text editor and `./scripts/build-apk.sh`.
If the app ever grows a real dependency — AndroidX, Compose, anything from
Maven — that trade flips and Gradle becomes correct.

**AndroidX / Material Components.** Would have supplied a nicer notification
builder and `FileProvider`. Both are replaceable in a few lines of framework
code (`Notification.Builder`, `PackageInstaller`), and taking them would have
made Gradle mandatory.

**`dataSync` as the foreground-service type.** The intuitive fit, and wrong:
from API 35 a `dataSync` service is capped at six cumulative hours per day,
after which the system calls `onTimeout()` and stops it. That is precisely fatal
for an always-on agent. `specialUse` carries no such timer. (Play Store review
would demand a justification for `specialUse`; wanctl is not distributed there.)

**A cgo/NDK build to fix DNS properly.** Still rejected, for the reasons in
ADR 0002. Being inside an app does not change that a cgo build costs a C
cross-compiler in the release pipeline and gives up static linking.

## Consequences

**`wanctl update` cannot work on Android any more, and this is a product change,
not an implementation detail.** The only directory the app may execute from is
owned by the package manager and read-only. So the unit of update is the APK,
and the installer is the system's. `wanctl update` now detects that it is
running from an APK and says so, instead of falling through to
`splitUpdateViaSudo` and reporting that it cannot find `sudo` — an answer to a
question nobody asked.

**The APK rides in the existing signed manifest as platform
`android`/`arm64.apk`.** The schema is untouched, deliberately: `VerifyManifest`
rejects a schema it does not know, and that check runs inside `wanctl update`
*before* it can update itself — so bumping the schema would strand every
already-installed client on every platform. The arch string `arm64.apk` yields
the name `wanctl-android-arm64.apk` under the existing
`Name == "wanctl-"+OS+"-"+Arch` rule, keeps the OS/Arch pair distinct from the
real `android/arm64` entry, and is unselectable by any older client because no
Go toolchain reports `GOARCH` as `arm64.apk`. `TestAndroidAPKRidesInTheExistingSchema`
pins all of it.

**A second signing key now matters.** The APK is signed with a dedicated
keystore (RSA 4096, `SHA256:4686 64C6 8E1B 50D0 3984 41CA 1125 B488 1C42 A5C5
E1C1 2094 0C1D 431C 08A9 BCE9`), separate from the release manifest keys because
Android's package manager and wanctl's manifest verification are two independent
trust decisions. **Losing it means every installed device must uninstall and
reinstall** — Android refuses an update signed by a different key, and there is
no recovery path that preserves app data.

**Releases now depend on the Android SDK.** `scripts/build-release.sh` refuses
to produce a release without the APK unless `WANCTL_SKIP_APK=1` says so out
loud, because a manifest with no APK entry silently strands every installed
Android app on its current version.

**The app reads the agent's stdout to decide when to stop retrying.** That is a
text coupling across a language boundary; `TestAgentErrorsTheAppKeysOn` in the
`wanctl` package pins the substrings so a reword fails CI instead of quietly
turning a permanent failure back into an infinite respawn loop.

**Surviving a reboot needs two mechanisms, not one.** `BOOT_COMPLETED` gets the
agent back in about a second, but it is best-effort: across four reboots of the
same build on the test device, one was discarded outright
(`am_broadcast_discard_app`) while other apps received the same broadcast in the
same second — Android drops a receiver whose process it cannot start, and a
just-booted tablet runs a load average of 30. Shipping on that alone would
reproduce the Termux failure (a device that quietly never comes back) with extra
steps, so a persisted 15-minute JobScheduler job acts as the floor. Both paths
depend on the battery-optimization exemption: the framework logs the permitted
foreground-service start as `SYSTEM_ALLOW_LISTED`, and without it there is no
start at all. The exemption is therefore a requirement, not a tuning tip, and
the UI and docs say so.

**The platform's guarantees stop at AOSP, and the device is an OEM device.** A
foreground service, a wake lock and the battery-optimisation exemption together
do not stop a vivo ROM from moving the app into `freezer:/frozen` two minutes
after the screen goes off — measured, with the poll loop not executing at all.
The vendor's own 后台耗电管理 setting is what decides it, and no code in this
repository can set it. That is a real limit on what an APK buys over Termux:
it removes the four exec workarounds and it solves reboot survival, but staying
alive on a phone is still partly a matter of what the user clicks in the
vendor's settings app. The app and the docs name the exact path rather than
implying the exemption button covers it.

**An app cannot write `/data/local/tmp`.** File transfers to an APK-hosted agent
land under the app's private directory instead. That is a narrowing against the
adb-pushed binary, and a good one — `/data/local/tmp` is shared with every other
app and with anyone holding adb — but it is a behaviour change for anyone
scripting against the old arrangement.
