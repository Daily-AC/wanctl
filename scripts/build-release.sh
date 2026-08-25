#!/bin/sh
set -eu

VERSION=${1:?usage: scripts/build-release.sh vMAJOR.MINOR.PATCH}
: "${WANCTL_RELEASE_SIGNING_KEY:?protected WANCTL_RELEASE_SIGNING_KEY is required}"

printf '%s\n' "$VERSION" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || {
  echo "invalid release version: $VERSION" >&2
  exit 1
}

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
DIST="$ROOT/release"
rm -rf "$DIST"
mkdir -p "$DIST"

CURRENT_KEY=$(cd "$ROOT" && go run ./cmd/release-manifest public-key)
TRUSTED_KEYS=$CURRENT_KEY
if [ -n "${WANCTL_RELEASE_PREVIOUS_PUBLIC_KEYS:-}" ]; then
  TRUSTED_KEYS="$CURRENT_KEY,$WANCTL_RELEASE_PREVIOUS_PUBLIC_KEYS"
fi
LDFLAGS="-s -w -X main.buildVersion=$VERSION -X wanctl/internal/release.TrustedPublicKeys=$TRUSTED_KEYS"

# Where the released artifacts will be downloadable from, flat (the project's
# GitHub releases for official builds). Baked into the binaries so `wanctl
# update` needs no relay-side mirror, and into the installers below.
RELEASE_BASE=${WANCTL_RELEASE_BASE:-}
if [ -n "$RELEASE_BASE" ]; then
  LDFLAGS="$LDFLAGS -X wanctl/internal/config.DefaultReleaseBase=$RELEASE_BASE"
fi

# The platform matrix and the per-target build environment live in one place,
# scripts/release-targets.sh, shared with the publisher's file-list check and
# CI's cross-compile gate. android/* is a distinct GOOS from linux/* on purpose:
# the Android build pulls in internal/androiddns, without which nothing
# resolves (Android has no /etc/resolv.conf and a CGO-free Go resolver falls
# back to 127.0.0.1). Serving a linux binary to a phone would install cleanly
# and then fail every dial.
. "$ROOT/scripts/release-targets.sh"
wanctl_targets | while read -r os arch; do
  echo "building $os/$arch"
  (cd "$ROOT" && wanctl_go_build "$os" "$arch" "$DIST/$(wanctl_artifact_name "$os" "$arch")" "$LDFLAGS")
done

# The Android APKs, one per ABI.
#
# They ride in the same signed manifest as everything else, as platforms
# android/<goarch>.apk — see internal/release.APKArch for why that spelling and
# not a schema change. platformFromName picks them up from the directory with
# no special case, so all that is needed here is to put them in place before
# the manifest is created.
#
# Not built inline, because they need the Android SDK and NDK. build-apk.sh
# leaves them under build/android/ for this script to pick up; a developer
# running this by hand gets them built on the spot. A release that would
# silently omit them is refused: the APK's SHA-256 in the manifest is the only
# thing an already-installed app can check an update against, so shipping
# without it strands every Android device on whatever version it already has.
APK_DIR="$ROOT/build/android"
apks_staged() {
  for arch in $(wanctl_apk_arches); do
    [ -f "$APK_DIR/wanctl-android-$arch.apk" ] || return 1
  done
}
if [ "${WANCTL_SKIP_APK:-}" = 1 ]; then
  echo "WARNING: skipping the Android APKs (WANCTL_SKIP_APK=1); installed Android apps will not see this release" >&2
else
  if apks_staged; then
    echo "using the Android APKs staged under $APK_DIR"
  else
    WANCTL_RELEASE_TRUSTED_KEYS="$TRUSTED_KEYS" "$ROOT/scripts/build-apk.sh" "$VERSION"
  fi
  for arch in $(wanctl_apk_arches); do
    cp "$APK_DIR/wanctl-android-$arch.apk" "$DIST/wanctl-android-$arch.apk"
  done
fi

(cd "$ROOT" && go run ./cmd/release-manifest create "$VERSION" "$DIST")
(cd "$ROOT" && go run ./cmd/release-manifest sign "$DIST")

PUB_PEM_FILE="$DIST/release-public.pem"
(cd "$ROOT" && go run ./cmd/release-manifest public-key-pem) > "$PUB_PEM_FILE"

# The installers verify the RSA signature, so they carry the RSA public key: as
# PEM for `openssl dgst` on Unix, as .NET XML for PowerShell, which cannot
# import PEM on 5.1.
RSA_PUB_PEM_FILE="$DIST/release-public-rsa.pem"
(cd "$ROOT" && go run ./cmd/release-manifest rsa-public-key-pem) > "$RSA_PUB_PEM_FILE"
RSA_PUB_XML=$(cd "$ROOT" && go run ./cmd/release-manifest rsa-public-key-xml)

# When configured, baking the relay into the installers lets `curl … | sh` and
# `irm … | iex` work against that relay's /dl mirror with nothing exported —
# the enterprise/intranet distribution shape. Official open-source builds leave
# it empty and bake RELEASE_BASE instead, so the installers pull straight from
# the release page.
DEFAULT_RELAY=$(cd "$ROOT" && go run ./cmd/release-manifest default-relay)
if [ -z "$DEFAULT_RELAY" ] && [ -z "$RELEASE_BASE" ]; then
  echo "WARNING: neither WANCTL_RELEASE_BASE nor a default relay is configured; install scripts will require WANCTL_DIST_BASE or WANCTL_RELAY to be set explicitly" >&2
fi

render_installer() {
  awk -v pem="$RSA_PUB_PEM_FILE" -v xml="$RSA_PUB_XML" -v relay="$DEFAULT_RELAY" -v rbase="$RELEASE_BASE" '
    $0 == "@WANCTL_RELEASE_RSA_PUBLIC_KEY_PEM@" { while ((getline line < pem) > 0) print line; close(pem); next }
    { gsub(/@WANCTL_RELEASE_RSA_PUBLIC_KEY_XML@/, xml); gsub(/@WANCTL_DEFAULT_RELAY@/, relay); gsub(/@WANCTL_RELEASE_BASE@/, rbase); print }
  ' "$1" > "$2"
}
render_installer "$ROOT/scripts/install.sh.in" "$DIST/install.sh"
render_installer "$ROOT/scripts/install.ps1.in" "$DIST/install.ps1"
chmod 0755 "$DIST/install.sh"

for placeholder_file in "$DIST/install.sh" "$DIST/install.ps1"; do
  if grep -q '@WANCTL_[A-Z_]*@' "$placeholder_file"; then
    echo "unsubstituted placeholder left in $placeholder_file" >&2
    grep -n '@WANCTL_[A-Z_]*@' "$placeholder_file" >&2
    exit 1
  fi
done

(cd "$ROOT" && go run ./cmd/release-manifest verify "$DIST" "$PUB_PEM_FILE")

echo "signed release $VERSION written to $DIST"
