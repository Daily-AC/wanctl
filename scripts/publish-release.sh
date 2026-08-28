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

# Completeness and consistency checks live in validate-release.sh so that
# release.yml runs the very same ones before `gh release create`
# (audit 2026-08-28, SEC-F-04).
"$ROOT/scripts/validate-release.sh" "$VERSION" "$DIST"

if (cd "$ROOT" && gh release view "$VERSION" >/dev/null 2>&1); then
  echo "release $VERSION already exists; refusing to overwrite it" >&2
  exit 1
fi

# GitHub Actions publishes directly from release.yml. This is the equivalent
# manual path, with the same artifact validation above plus explicit notes.
(cd "$ROOT" && gh release create "$VERSION" "$DIST"/* \
  --title "wanctl $VERSION" --notes-file "$NOTES" --verify-tag)
