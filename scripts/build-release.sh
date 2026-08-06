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

# android/arm64 is a distinct GOOS from linux/arm64 on purpose: the Android
# build pulls in internal/androiddns, without which nothing resolves (Android
# has no /etc/resolv.conf and a CGO-free Go resolver falls back to 127.0.0.1).
# Serving a linux/arm64 binary to a phone would install cleanly and then fail
# every dial.
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 android/arm64; do
  os=${target%/*}
  arch=${target#*/}
  suffix=
  [ "$os" = windows ] && suffix=.exe
  (cd "$ROOT" && env -u WANCTL_RELEASE_SIGNING_KEY CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "$LDFLAGS" -o "$DIST/wanctl-$os-$arch$suffix" .)
done

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

# Baking the relay in is what lets `curl … | sh` and `irm … | iex` work without
# the caller exporting WANCTL_RELAY first. It narrows nothing: the script is
# fetched from that same relay.
DEFAULT_RELAY=$(cd "$ROOT" && go run ./cmd/release-manifest default-relay)

render_installer() {
  awk -v pem="$RSA_PUB_PEM_FILE" -v xml="$RSA_PUB_XML" -v relay="$DEFAULT_RELAY" '
    $0 == "@WANCTL_RELEASE_RSA_PUBLIC_KEY_PEM@" { while ((getline line < pem) > 0) print line; close(pem); next }
    { gsub(/@WANCTL_RELEASE_RSA_PUBLIC_KEY_XML@/, xml); gsub(/@WANCTL_DEFAULT_RELAY@/, relay); print }
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
