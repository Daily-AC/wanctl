#!/bin/sh
set -eu

VERSION=${1:?usage: scripts/publish-release.sh vMAJOR.MINOR.PATCH DIST_DIR NOTES_FILE}
DIST=${2:?usage: scripts/publish-release.sh vMAJOR.MINOR.PATCH DIST_DIR NOTES_FILE}
NOTES=${3:-}

ROOT=$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)
. "$ROOT/scripts/release-version.sh"
KIND=$(wanctl_release_kind "$VERSION")
if [ "$KIND" = stable ]; then
  NOTES=${NOTES:-"$ROOT/internal/portal/changelog/$VERSION.md"}
  test -f "$NOTES" || { echo "release notes not found: $NOTES" >&2; exit 1; }
fi
command -v gh >/dev/null 2>&1 || { echo "gh is required" >&2; exit 1; }

DIST=$(CDPATH='' cd -- "$DIST" && pwd)

TAG_COMMIT=$(cd "$ROOT" && git rev-parse "$VERSION^{commit}")
HEAD_COMMIT=$(cd "$ROOT" && git rev-parse HEAD)
if [ "$TAG_COMMIT" != "$HEAD_COMMIT" ]; then
  echo "$VERSION points to $TAG_COMMIT but the publisher source is $HEAD_COMMIT" >&2
  exit 1
fi

# Completeness and consistency checks live in validate-release.sh so that
# release.yml runs the very same ones before `gh release create`
# (audit 2026-08-28, SEC-F-04).
(cd "$ROOT" && ./scripts/check-promotion.sh "$VERSION")
"$ROOT/scripts/validate-release.sh" "$VERSION" "$DIST"

if (cd "$ROOT" && gh release view "$VERSION" >/dev/null 2>&1); then
  echo "release $VERSION already exists; refusing to overwrite it" >&2
  exit 1
fi

# GitHub Actions publishes directly from release.yml. This is the equivalent
# manual path, with the same artifact validation above plus explicit notes.
if [ "$KIND" = beta ]; then
  (cd "$ROOT" && gh release create "$VERSION" "$DIST"/* \
    --title "wanctl $VERSION" --prerelease --latest=false --generate-notes --verify-tag)
else
  (cd "$ROOT" && gh release create "$VERSION" "$DIST"/* \
    --title "wanctl $VERSION" --latest --notes-file "$NOTES" --verify-tag)
fi
