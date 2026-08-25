#!/bin/sh
# Builds every release target exactly as build-release.sh would, minus the
# version and trust anchors, so CI catches a platform that stopped compiling —
# or an NDK that went missing — before a tag does.
#
# Usage: scripts/cross-compile.sh [OUT_DIR]
set -eu

ROOT=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
. "$ROOT/scripts/release-targets.sh"

OUT=${1:-$(mktemp -d)}
mkdir -p "$OUT"
cd "$ROOT"

wanctl_targets | while read -r os arch; do
  name=$(wanctl_artifact_name "$os" "$arch")
  echo "building $os/$arch -> $OUT/$name"
  wanctl_go_build "$os" "$arch" "$OUT/$name" "-s -w"
done

echo "all release targets compile:"
wanctl_targets | tr ' ' / | tr '\n' ' '
echo
