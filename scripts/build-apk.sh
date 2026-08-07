#!/bin/sh
# Builds the Android APK that carries the wanctl agent.
#
# Deliberately not Gradle. The chain here is aapt2 → javac → d8 → zipalign →
# apksigner, all of which ship inside the Android SDK, so the build needs no
# network at all. That is not minimalism for its own sake: this project's own CI
# already runs on a runner that cannot reach proxy.golang.org (see
# .gitlab-ci.yml), and adding a build system that resolves a dependency graph
# from Maven Central on every invocation would put the Android artifact behind
# the flakiest link in the pipeline. The app is a few hundred lines of framework
# Java with no AndroidX, so there is nothing for a dependency resolver to do.
#
# The cost, stated plainly: you cannot open this in Android Studio as a project.
# See docs/adr/0003-android-apk.md.
set -eu

VERSION=${1:-dev}
ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
APP="$ROOT/android"
OUT=${WANCTL_APK_OUT:-"$ROOT/build/android"}

if [ "$VERSION" != "dev" ]; then
  printf '%s\n' "$VERSION" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || {
    echo "invalid release version: $VERSION" >&2
    exit 1
  }
fi

# ---------------------------------------------------------------- toolchain

for tool in zip unzip find go; do
  command -v "$tool" >/dev/null 2>&1 || { echo "missing required tool: $tool" >&2; exit 1; }
done

SDK=${ANDROID_HOME:-${ANDROID_SDK_ROOT:-"$HOME/Library/Android/sdk"}}
[ -d "$SDK" ] || { echo "Android SDK not found (set ANDROID_HOME); looked in $SDK" >&2; exit 1; }

# Newest build-tools wins; any of them can build this, and pinning one would
# mean editing this file every time the SDK updates.
BT_DIR=$(ls -1 "$SDK/build-tools" 2>/dev/null | sort -V | tail -1)
[ -n "$BT_DIR" ] || { echo "no build-tools in $SDK/build-tools" >&2; exit 1; }
BT="$SDK/build-tools/$BT_DIR"

PLATFORM_DIR=$(ls -1 "$SDK/platforms" 2>/dev/null | sort -V | tail -1)
[ -n "$PLATFORM_DIR" ] || { echo "no platforms in $SDK/platforms" >&2; exit 1; }
ANDROID_JAR="$SDK/platforms/$PLATFORM_DIR/android.jar"

# apksigner and d8 are Java programs. Android Studio bundles a JDK, which is the
# one every developer with an SDK already has.
if [ -z "${JAVA_HOME:-}" ]; then
  for candidate in \
      "/Applications/Android Studio.app/Contents/jbr/Contents/Home" \
      "$SDK/../jbr" ; do
    [ -x "$candidate/bin/javac" ] && { JAVA_HOME=$candidate; break; }
  done
fi
if [ -z "${JAVA_HOME:-}" ]; then
  command -v javac >/dev/null 2>&1 || { echo "no JDK: set JAVA_HOME" >&2; exit 1; }
else
  export JAVA_HOME
  PATH="$JAVA_HOME/bin:$PATH"
  export PATH
fi

echo "  sdk        $SDK"
echo "  build-tools $BT_DIR   platform $PLATFORM_DIR"
echo "  jdk        $(javac -version 2>&1)"

# ---------------------------------------------------------------- version

# Android needs a monotonically increasing integer. Derived from the tag so it
# can never disagree with the version string: v1.2.3 -> 10203.
if [ "$VERSION" = "dev" ]; then
  VERSION_CODE=1
  VERSION_NAME=dev
else
  V=${VERSION#v}
  MAJOR=${V%%.*}
  REST=${V#*.}
  MINOR=${REST%%.*}
  PATCH=${REST#*.}
  VERSION_CODE=$((MAJOR * 10000 + MINOR * 100 + PATCH))
  VERSION_NAME=$VERSION
fi

rm -rf "$OUT"
mkdir -p "$OUT/gen" "$OUT/classes" "$OUT/dex" "$OUT/flat" "$OUT/lib/arm64-v8a"

# ---------------------------------------------------------------- the binary

# Same flags as scripts/build-release.sh so the binary in the APK is the binary
# that would have been published as wanctl-android-arm64. The name is the point:
# only files under lib/<abi>/ are extracted to nativeLibraryDir with the
# executable bit and the apk_data_file label that untrusted_app may exec.
#
# The trusted release keys have to be baked in or the binary inside the APK can
# verify nothing — and the one thing it is asked to verify is the APK of the
# next release. A release build without them is not a degraded build, it is one
# whose in-app updater is permanently broken, so refuse rather than ship it.
# build-release.sh passes them; standalone callers get them derived from the
# signing key if they hold it.
TRUSTED=${WANCTL_RELEASE_TRUSTED_KEYS:-}
if [ -z "$TRUSTED" ] && [ -n "${WANCTL_RELEASE_SIGNING_KEY:-}" ]; then
  TRUSTED=$(cd "$ROOT" && go run ./cmd/release-manifest public-key)
  if [ -n "${WANCTL_RELEASE_PREVIOUS_PUBLIC_KEYS:-}" ]; then
    TRUSTED="$TRUSTED,$WANCTL_RELEASE_PREVIOUS_PUBLIC_KEYS"
  fi
fi
if [ "$VERSION" != dev ] && [ -z "$TRUSTED" ]; then
  echo "refusing to build $VERSION without release trust anchors:" >&2
  echo "  set WANCTL_RELEASE_TRUSTED_KEYS, or WANCTL_RELEASE_SIGNING_KEY to derive them." >&2
  echo "  (an APK built without them can never verify an update)" >&2
  exit 1
fi

LDFLAGS="-s -w -X main.buildVersion=$VERSION_NAME"
if [ -n "$TRUSTED" ]; then
  LDFLAGS="$LDFLAGS -X wanctl/internal/release.TrustedPublicKeys=$TRUSTED"
fi
echo "  building wanctl android/arm64 …"
(cd "$ROOT" && env -u WANCTL_RELEASE_SIGNING_KEY CGO_ENABLED=0 GOOS=android GOARCH=arm64 \
    go build -trimpath -ldflags "$LDFLAGS" -o "$OUT/lib/arm64-v8a/libwanctl.so" .)

# ---------------------------------------------------------------- generated source

# The portal origin is compiled into the Go binary. Reading it out of the same
# constant, rather than repeating it in Java, is what keeps the app's "log in"
# button pointed at the same place the binary enrolls against.
PORTAL=$(sed -n 's/.*DefaultPortal *= *"\([^"]*\)".*/\1/p' "$ROOT/internal/config/config.go" | head -1)
[ -n "$PORTAL" ] || { echo "could not read DefaultPortal from internal/config/config.go" >&2; exit 1; }
echo "  portal     $PORTAL"

mkdir -p "$OUT/gen/com/***REMOVED***/wanctl"
cat > "$OUT/gen/com/***REMOVED***/wanctl/BuildInfo.java" <<EOF
package com.***REMOVED***.wanctl;

/** Generated by scripts/build-apk.sh — do not edit. */
final class BuildInfo {
    static final String PORTAL = "$PORTAL";
    static final String VERSION = "$VERSION_NAME";

    private BuildInfo() {
    }
}
EOF

# ---------------------------------------------------------------- resources

echo "  aapt2 compile/link …"
"$BT/aapt2" compile --dir "$APP/res" -o "$OUT/flat/res.zip"
"$BT/aapt2" link \
    -I "$ANDROID_JAR" \
    --manifest "$APP/AndroidManifest.xml" \
    --java "$OUT/gen" \
    --version-code "$VERSION_CODE" \
    --version-name "$VERSION_NAME" \
    --auto-add-overlay \
    -o "$OUT/base.apk" \
    "$OUT/flat/res.zip"

# ---------------------------------------------------------------- java -> dex

echo "  javac …"
find "$APP/java" "$OUT/gen" -name '*.java' > "$OUT/sources.txt"
# -source/-target rather than --release: --release pins the *JDK's* platform
# classes, and here the platform is android.jar. -Xlint:-options silences the
# bootstrap-classpath warning that pairing produces; it is not piped through
# grep because a pipeline would hide javac's own exit status under `set -e`.
javac -Xlint:-options -source 17 -target 17 \
    -encoding UTF-8 \
    -classpath "$ANDROID_JAR" \
    -d "$OUT/classes" \
    @"$OUT/sources.txt"

echo "  d8 …"
find "$OUT/classes" -name '*.class' > "$OUT/classes.txt"
"$BT/d8" --release --lib "$ANDROID_JAR" --min-api 29 \
    --output "$OUT/dex" @"$OUT/classes.txt"

# ---------------------------------------------------------------- package

echo "  packaging …"
cp "$OUT/base.apk" "$OUT/unsigned.apk"
(cd "$OUT/dex" && zip -q -X "$OUT/unsigned.apk" classes.dex)
# Deflate the binary: extractNativeLibs="true" means the installer decompresses
# it onto disk anyway, and storing it raw would add 10 MB to every download.
(cd "$OUT" && zip -q -X "$OUT/unsigned.apk" lib/arm64-v8a/libwanctl.so)

"$BT/zipalign" -f -p 4 "$OUT/unsigned.apk" "$OUT/aligned.apk"

# ---------------------------------------------------------------- signing

# A release keystore is passed in as base64 so it can live in a CI variable.
# Without one this falls back to the debug key, which produces an APK that
# installs on a developer's own device and is worthless to anyone else — said
# out loud, because an unsigned-in-practice APK that looks finished is exactly
# the kind of thing that gets published.
KS="$OUT/signing.jks"
if [ -n "${WANCTL_ANDROID_KEYSTORE_B64:-}" ]; then
  printf '%s' "$WANCTL_ANDROID_KEYSTORE_B64" | base64 -d > "$KS"
  KS_PASS=${WANCTL_ANDROID_KEYSTORE_PASS:?WANCTL_ANDROID_KEYSTORE_PASS is required with a keystore}
  KEY_ALIAS=${WANCTL_ANDROID_KEY_ALIAS:-wanctl}
  KEY_PASS=${WANCTL_ANDROID_KEY_PASS:-$KS_PASS}
  SIGNED_WITH=release
elif [ -n "${WANCTL_ANDROID_KEYSTORE:-}" ]; then
  cp "$WANCTL_ANDROID_KEYSTORE" "$KS"
  KS_PASS=${WANCTL_ANDROID_KEYSTORE_PASS:?WANCTL_ANDROID_KEYSTORE_PASS is required with a keystore}
  KEY_ALIAS=${WANCTL_ANDROID_KEY_ALIAS:-wanctl}
  KEY_PASS=${WANCTL_ANDROID_KEY_PASS:-$KS_PASS}
  SIGNED_WITH=release
else
  [ -f "$HOME/.android/debug.keystore" ] || {
    echo "no release keystore (WANCTL_ANDROID_KEYSTORE[_B64]) and no debug keystore" >&2
    exit 1
  }
  cp "$HOME/.android/debug.keystore" "$KS"
  KS_PASS=android
  KEY_ALIAS=androiddebugkey
  KEY_PASS=android
  SIGNED_WITH=debug
fi

APK="$OUT/wanctl-android-arm64.apk"
"$BT/apksigner" sign \
    --ks "$KS" --ks-pass "pass:$KS_PASS" \
    --ks-key-alias "$KEY_ALIAS" --key-pass "pass:$KEY_PASS" \
    --out "$APK" "$OUT/aligned.apk"
rm -f "$KS" "$OUT/aligned.apk" "$OUT/unsigned.apk" "$OUT/base.apk"
"$BT/apksigner" verify "$APK"

# The one property the whole design rests on. If the .so is not in the APK at
# lib/<abi>/, or extractNativeLibs is off, the installed app has nothing to
# exec — and that failure would otherwise surface on a device as a bare
# "Permission denied" that reads like an SELinux denial.
if ! unzip -l "$APK" | grep -q 'lib/arm64-v8a/libwanctl.so'; then
  echo "FATAL: libwanctl.so missing from the APK" >&2
  exit 1
fi
# Assert the value, not merely the attribute's presence: false is the Android
# Gradle Plugin's default and reads almost identically in a diff, and getting it
# wrong yields an installed app with an empty nativeLibraryDir — which surfaces
# on the device as a bare "Permission denied" that reads like an SELinux denial.
# Older aapt2 renders a true boolean as 0xffffffff, newer ones as `true`.
EXTRACT=$("$BT/aapt2" dump xmltree "$APK" --file AndroidManifest.xml \
    | grep 'extractNativeLibs' | head -1)
case "$EXTRACT" in
  *=true | *0xffffffff) ;;
  *)
    echo "FATAL: extractNativeLibs is not true (${EXTRACT:-attribute absent});" >&2
    echo "       nativeLibraryDir would be empty and there would be nothing to exec" >&2
    exit 1
    ;;
esac

echo
echo "signed with the $SIGNED_WITH key: $APK"
if [ "$SIGNED_WITH" = debug ]; then
  echo "WARNING: debug key — installable on your own device only, DO NOT distribute" >&2
fi
ls -l "$APK"
