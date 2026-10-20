#!/bin/sh
# Run in the repository whose tags are being released; no network is needed.
set -eu

VERSION=${1:?usage: scripts/check-promotion.sh TAG}
ROOT=$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)
. "$ROOT/scripts/release-version.sh"
fail() {
  echo "$*; see docs/releasing.md" >&2
  exit 1
}
KIND=$(wanctl_release_kind "$VERSION") || fail "invalid tag $VERSION"
[ "$KIND" = stable ] || exit 0

git rev-parse --verify "refs/tags/$VERSION^{commit}" >/dev/null || fail "stable tag $VERSION does not exist"
# Version sorting compares the numeric suffix, so beta.10 outranks beta.9.
BETA=$(git tag --list "$VERSION-beta.*" --sort=-version:refname |
  while IFS= read -r tag; do
    if wanctl_release_kind "$tag" >/dev/null 2>&1; then printf '%s\n' "$tag"; fi
  done | head -n 1)
[ -n "$BETA" ] || fail "$VERSION has no beta to promote"
CHANGED=$(git diff --name-only "$BETA" "$VERSION") || fail "cannot compare $BETA and $VERSION"
UNEXPECTED=$(printf '%s\n' "$CHANGED" | grep -Ev '^(internal/portal/changelog/.*)?$' || true)
[ -z "$UNEXPECTED" ] || fail "$VERSION differs from $BETA outside internal/portal/changelog/:
$UNEXPECTED"
echo "$VERSION promotes $BETA"
