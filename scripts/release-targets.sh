#!/bin/sh
# The one list of platforms a wanctl release ships, and how each one is built.
#
# Sourced — not executed — by build-release.sh, build-apk.sh,
# publish-release.sh and cross-compile.sh. Nothing else may carry its own copy
# of the matrix: the v0.1.3 release shipped with a publish-time file list that
# had drifted from the build loop, and the mismatch was found by the publisher
# refusing to publish, which is the good outcome. The bad one is a target
# quietly dropping out of the installers' reach.
#
# After sourcing:
#   wanctl_targets                        "os arch" per line, every binary shipped
#   wanctl_apk_arches                     the GOARCH of every APK shipped
#   wanctl_artifact_name OS ARCH          wanctl-OS-ARCH, plus .exe on windows
#   wanctl_go_build OS ARCH OUT LDFLAGS   build one target into OUT (run from the repo root)
#   wanctl_android_cc ARCH                the NDK clang that links a cgo Android target

wanctl_targets() {
  # Everything a modern machine could be, plus the 32-bit population that has
  # not gone away: 32-bit Windows installs, armv6/v7 single-board computers and
  # NAS boxes, MIPS routers. macOS has no 32-bit target — Go dropped darwin/386
  # in 1.15, and Catalina stopped running 32-bit binaries a year earlier.
  cat <<'EOF'
linux amd64
linux arm64
linux 386
linux arm
linux mips
linux mipsle
darwin amd64
darwin arm64
windows amd64
windows 386
windows arm64
android arm64
android arm
android 386
android amd64
EOF
}

# One APK per ABI rather than one universal APK: a universal one would be four
# binaries deep and every in-app update would download all four to use one.
# Each APK carries exactly the lib/<abi>/ its GOARCH maps to, and rides in the
# manifest as android/<goarch>.apk (see internal/release.APKArch).
wanctl_apk_arches() {
  printf '%s\n' arm64 arm 386 amd64
}

# The Android ABI directory the package manager extracts for a GOARCH.
wanctl_android_abi() {
  case "$1" in
    arm64) echo arm64-v8a ;;
    arm) echo armeabi-v7a ;;
    386) echo x86 ;;
    amd64) echo x86_64 ;;
    *) echo "no Android ABI for GOARCH $1" >&2; return 1 ;;
  esac
}

wanctl_artifact_name() {
  case "$1" in
    windows) printf 'wanctl-%s-%s.exe\n' "$1" "$2" ;;
    *) printf 'wanctl-%s-%s\n' "$1" "$2" ;;
  esac
}

# Where the NDK is. GitHub's runners export ANDROID_NDK_LATEST_HOME; Android
# Studio installs under <sdk>/ndk/<version>; anyone else sets ANDROID_NDK_HOME.
wanctl_ndk_root() {
  for d in "${ANDROID_NDK_HOME:-}" "${ANDROID_NDK_ROOT:-}" "${ANDROID_NDK_LATEST_HOME:-}" "${ANDROID_NDK:-}"; do
    if [ -n "$d" ] && [ -d "$d/toolchains/llvm/prebuilt" ]; then
      printf '%s\n' "$d"
      return 0
    fi
  done
  sdk=${ANDROID_HOME:-${ANDROID_SDK_ROOT:-"$HOME/Library/Android/sdk"}}
  if [ -d "$sdk/ndk" ]; then
    latest=$(ls -1 "$sdk/ndk" 2>/dev/null | sort -V | tail -1)
    if [ -n "$latest" ] && [ -d "$sdk/ndk/$latest/toolchains/llvm/prebuilt" ]; then
      printf '%s\n' "$sdk/ndk/$latest"
      return 0
    fi
  fi
  echo "Android NDK not found: set ANDROID_NDK_HOME (needed to link android/arm, android/386 and android/amd64)" >&2
  return 1
}

# Only android/arm64 links internally in Go; the other three Android targets
# need the platform's own linker, which means clang from the NDK with cgo on.
# The resulting binaries are dynamically linked against bionic — fine, that is
# the one libc every Android device has — and PIE, which Android requires.
# The API level matches minSdkVersion in android/AndroidManifest.xml.
wanctl_android_cc() {
  ndk=$(wanctl_ndk_root) || return 1
  host=$(ls -1 "$ndk/toolchains/llvm/prebuilt" | head -1)
  case "$1" in
    arm64) triple=aarch64-linux-android ;;
    arm) triple=armv7a-linux-androideabi ;;
    386) triple=i686-linux-android ;;
    amd64) triple=x86_64-linux-android ;;
    *) echo "no Android target for GOARCH $1" >&2; return 1 ;;
  esac
  cc="$ndk/toolchains/llvm/prebuilt/$host/bin/$triple${WANCTL_ANDROID_API:-29}-clang"
  if [ ! -x "$cc" ]; then
    echo "NDK compiler missing: $cc" >&2
    return 1
  fi
  printf '%s\n' "$cc"
}

# wanctl_go_build OS ARCH OUT LDFLAGS — one `go build`, with the per-target
# environment that makes the binary run where it is meant to:
#   linux/arm      GOARM=6: runs on armv6 (Pi Zero/1) and every armv7; armv5 is not served.
#   linux/mips*    GOMIPS=softfloat: routers routinely have no FPU, and a
#                  softfloat binary runs on the ones that do.
#   android/arm    GOARM=7, the only ARM Android supports.
#   android/!arm64 cgo through the NDK (see wanctl_android_cc).
# The signing keys are scrubbed from the build environment: a binary must
# never be able to observe the key that will sign it.
wanctl_go_build() {
  os=$1
  arch=$2
  out=$3
  ldflags=$4
  goarm=
  gomips=
  cgo=0
  case "$os/$arch" in
    linux/arm) goarm=6 ;;
    linux/mips | linux/mipsle) gomips=softfloat ;;
    android/arm) goarm=7; cgo=1 ;;
    android/386 | android/amd64) cgo=1 ;;
  esac
  if [ "$cgo" = 1 ]; then
    cc=$(wanctl_android_cc "$arch") || return 1
    env -u WANCTL_RELEASE_SIGNING_KEY -u WANCTL_RELEASE_RSA_KEY \
      CGO_ENABLED=1 CC="$cc" GOOS="$os" GOARCH="$arch" GOARM="$goarm" \
      go build -trimpath -ldflags "$ldflags" -o "$out" .
  else
    env -u WANCTL_RELEASE_SIGNING_KEY -u WANCTL_RELEASE_RSA_KEY \
      CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" GOARM="$goarm" GOMIPS="$gomips" \
      go build -trimpath -ldflags "$ldflags" -o "$out" .
  fi
}
