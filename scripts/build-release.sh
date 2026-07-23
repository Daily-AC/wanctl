#!/bin/sh
set -eu

VERSION=${1:?usage: scripts/build-release.sh vMAJOR.MINOR.PATCH}
: "${WANCTL_RELEASE_SIGNING_KEY:?protected WANCTL_RELEASE_SIGNING_KEY is required}"

case "$VERSION" in
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *) echo "invalid release version: $VERSION" >&2; exit 1 ;;
esac

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

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
  os=${target%/*}
  arch=${target#*/}
  suffix=
  [ "$os" = windows ] && suffix=.exe
  (cd "$ROOT" && env -u WANCTL_RELEASE_SIGNING_KEY CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "$LDFLAGS" -o "$DIST/wanctl-$os-$arch$suffix" .)
done

(cd "$ROOT" && go run ./cmd/release-manifest create "$VERSION" "$DIST")
(cd "$ROOT" && go run ./cmd/release-manifest sign "$DIST")

PUB_PEM_FILE="$DIST/.release-public.pem"
(cd "$ROOT" && go run ./cmd/release-manifest public-key-pem) > "$PUB_PEM_FILE"
awk -v pem="$PUB_PEM_FILE" '$0 == "@WANCTL_RELEASE_PUBLIC_KEY_PEM@" { while ((getline line < pem) > 0) print line; close(pem); next } { print }' \
  "$ROOT/scripts/install.sh.in" > "$DIST/install.sh"
awk -v pem="$PUB_PEM_FILE" '$0 == "@WANCTL_RELEASE_PUBLIC_KEY_PEM@" { while ((getline line < pem) > 0) print line; close(pem); next } { print }' \
  "$ROOT/scripts/install.ps1.in" > "$DIST/install.ps1"
rm -f "$PUB_PEM_FILE"
chmod 0755 "$DIST/install.sh"

echo "signed release $VERSION written to $DIST"
