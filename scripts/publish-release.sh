#!/bin/sh
set -eu

VERSION=${1:?usage: scripts/publish-release.sh vMAJOR.MINOR.PATCH DIST_DIR NOTES_FILE}
DIST=${2:?usage: scripts/publish-release.sh vMAJOR.MINOR.PATCH DIST_DIR NOTES_FILE}
NOTES=${3:?usage: scripts/publish-release.sh vMAJOR.MINOR.PATCH DIST_DIR NOTES_FILE}

printf '%s\n' "$VERSION" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || {
  echo "invalid release version: $VERSION" >&2
  exit 1
}
test -f "$NOTES" || { echo "release notes not found: $NOTES" >&2; exit 1; }
command -v gh >/dev/null 2>&1 || { echo "gh is required" >&2; exit 1; }

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
DIST=$(CDPATH= cd -- "$DIST" && pwd)

TAG_COMMIT=$(cd "$ROOT" && git rev-parse "$VERSION^{commit}")
HEAD_COMMIT=$(cd "$ROOT" && git rev-parse HEAD)
if [ "$TAG_COMMIT" != "$HEAD_COMMIT" ]; then
  echo "$VERSION points to $TAG_COMMIT but the publisher source is $HEAD_COMMIT" >&2
  exit 1
fi

# The expected file list is derived from the same matrix the build loop runs
# over, so the two cannot drift; what this check still catches is a build that
# stopped short, a stray file, or an APK that was skipped.
. "$ROOT/scripts/release-targets.sh"
EXPECTED=$({
  printf '%s\n' install.ps1 install.sh manifest.json manifest.json.rsa.sig manifest.json.sig release-public-rsa.pem release-public.pem
  wanctl_targets | while read -r os arch; do wanctl_artifact_name "$os" "$arch"; done
  wanctl_apk_arches | while read -r arch; do echo "wanctl-android-$arch.apk"; done
} | LC_ALL=C sort)
ACTUAL=$(find "$DIST" -mindepth 1 -maxdepth 1 -exec basename {} \; | LC_ALL=C sort)
if [ "$ACTUAL" != "$EXPECTED" ]; then
  echo "release directory has missing or unexpected files" >&2
  printf 'expected:\n%s\nactual:\n%s\n' "$EXPECTED" "$ACTUAL" >&2
  exit 1
fi

(cd "$ROOT" && go run ./cmd/release-manifest verify "$DIST" "$DIST/release-public.pem" "$DIST/release-public-rsa.pem")
MANIFEST_VERSION=$(sed -n 's/^[[:space:]]*"version":[[:space:]]*"\([^"]*\)",[[:space:]]*$/\1/p' "$DIST/manifest.json")
test "$MANIFEST_VERSION" = "$VERSION" || {
  echo "manifest version $MANIFEST_VERSION does not match tag $VERSION" >&2
  exit 1
}

# The installers verify the RSA signature, so each must carry the release's own
# RSA public key — as PEM for `openssl dgst` on Unix, as .NET XML for
# PowerShell, which cannot import PEM on 5.1. An installer built against a
# different key would fail for every new user while `wanctl update` kept working.
TMPDIR_WANCTL=$(mktemp -d)
trap 'rm -rf "$TMPDIR_WANCTL"' EXIT HUP INT TERM
sed -n '/^-----BEGIN PUBLIC KEY-----$/,/^-----END PUBLIC KEY-----$/p' "$DIST/install.sh" > "$TMPDIR_WANCTL/install.sh.pem"
cmp -s "$DIST/release-public-rsa.pem" "$TMPDIR_WANCTL/install.sh.pem" || {
  echo "install.sh does not embed release-public-rsa.pem" >&2
  exit 1
}
EXPECTED_XML=$(cd "$ROOT" && go run ./cmd/release-manifest rsa-public-key-xml "$DIST/release-public-rsa.pem")
grep -qF "$EXPECTED_XML" "$DIST/install.ps1" || {
  echo "install.ps1 does not embed the release RSA public key" >&2
  exit 1
}

if (cd "$ROOT" && gh release view "$VERSION" >/dev/null 2>&1); then
  echo "release $VERSION already exists; refusing to overwrite it" >&2
  exit 1
fi

# GitHub Actions publishes directly from release.yml. This is the equivalent
# manual path, with the same artifact validation above plus explicit notes.
(cd "$ROOT" && gh release create "$VERSION" "$DIST"/* \
  --title "wanctl $VERSION" --notes-file "$NOTES" --verify-tag)
