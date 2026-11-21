#!/bin/sh
# Prints the path of a `go` that can still build for Windows 7, fetching it
# into a cache on first use.
#
# Go 1.21 dropped Windows 7 and 8.x, and from then on the linker stamps every
# Windows binary with OS and subsystem version 10.0: the Windows 7 loader
# refuses such a file outright ("not a valid application for this OS
# platform"). thongtech/go-legacy-win7 is upstream Go with the removed
# Windows 7 support restored, and it links 6.1 binaries. Exactly one release
# artifact, windows/amd64.win7, is built with it (see wanctl_go_build in
# release-targets.sh); everything else keeps the official toolchain.
#
# The toolchain is pinned by version and by the SHA-256 of each host's
# archive, taken from the upstream release's asset digests. The archive is
# checked before anything is unpacked, and a host with no pinned digest is
# refused rather than served an unverified download. Bumping the pin is
# described in docs/releasing.md.
#
# Usage: scripts/go-legacy-win7.sh        (prints the path to its bin/go)
# Cache: $WANCTL_GO_LEGACY_WIN7_CACHE, default <user cache dir>/wanctl/go-legacy-win7
set -eu

VERSION=1.26.11-1

case "$(uname -s)" in
  Darwin) host_os=darwin ;;
  Linux) host_os=linux ;;
  *) host_os=$(uname -s) ;;
esac
case "$(uname -m)" in
  arm64 | aarch64) host_arch=arm64 ;;
  x86_64 | amd64) host_arch=amd64 ;;
  *) host_arch=$(uname -m) ;;
esac
host="${host_os}_${host_arch}"
case "$host" in
  darwin_arm64) sha256=60996604cc7fe8c89f1ee218050def2c9bdf562ca860113efa6deb090394f463 ;;
  darwin_amd64) sha256=315e82d5b77b4bb72dcd8d5c44fd8e20c7a6e73492a68fe09e80c0447df988f1 ;;
  linux_amd64) sha256=b3aaa2206a79a62d0552485d60240e512555cec701edd4fc0b5ff45644e0c21f ;;
  linux_arm64) sha256=88430c039eef696adb20111076c04bbb51286b93147b281e4d3073d87727bffe ;;
  *)
    echo "go-legacy-win7 $VERSION has no pinned archive for build host $host;" >&2
    echo "the windows/amd64.win7 artifact can only be built on darwin or linux, amd64 or arm64" >&2
    exit 1
    ;;
esac

if [ -n "${WANCTL_GO_LEGACY_WIN7_CACHE:-}" ]; then
  cache=$WANCTL_GO_LEGACY_WIN7_CACHE
elif [ "$host_os" = darwin ]; then
  cache="$HOME/Library/Caches/wanctl/go-legacy-win7"
else
  cache="${XDG_CACHE_HOME:-$HOME/.cache}/wanctl/go-legacy-win7"
fi

# One directory per version and host, holding the unpacked tree and the
# digest it was verified against. A directory whose recorded digest is not
# today's pin (an older pin, an interrupted unpack) is fetched again.
dir="$cache/go-legacy-win7-$VERSION.$host"
gobin="$dir/go-legacy-win7/bin/go"
if [ -x "$gobin" ] && [ "$(cat "$dir/.sha256" 2>/dev/null)" = "$sha256" ]; then
  printf '%s\n' "$gobin"
  exit 0
fi

archive="go-legacy-win7-$VERSION.$host.tar.gz"
url="https://github.com/thongtech/go-legacy-win7/releases/download/v$VERSION/$archive"
mkdir -p "$cache"
work=$(mktemp -d "$cache/.fetch.XXXXXX")
trap 'rm -rf "$work"' EXIT HUP INT TERM

echo "fetching $url" >&2
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 \
  --output "$work/$archive" "$url"

if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$work/$archive" | cut -d' ' -f1)
else
  got=$(shasum -a 256 "$work/$archive" | cut -d' ' -f1)
fi
if [ "$got" != "$sha256" ]; then
  echo "$archive: SHA-256 $got does not match the pinned $sha256; refusing to unpack it" >&2
  exit 1
fi

mkdir "$work/tree"
tar -xzf "$work/$archive" -C "$work/tree"
[ -x "$work/tree/go-legacy-win7/bin/go" ] || {
  echo "$archive did not unpack to go-legacy-win7/bin/go" >&2
  exit 1
}
printf '%s\n' "$sha256" > "$work/tree/.sha256"
rm -rf "$dir"
mv "$work/tree" "$dir"
# mv into a directory that reappeared (a second fetch racing this one) nests
# the tree instead of placing it; say so rather than print a path that is not there.
[ -x "$gobin" ] || { echo "could not place the toolchain at $dir; run again" >&2; exit 1; }
printf '%s\n' "$gobin"
