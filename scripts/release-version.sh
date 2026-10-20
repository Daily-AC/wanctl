#!/bin/sh
# Shared grammar for builders, validation and promotion. APKs remain stable-only.
wanctl_release_kind() {
  case "$1" in
    *[!v0-9.beta-]*) echo "invalid release version: $1" >&2; return 1 ;;
  esac
  printf '%s\n' "$1" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-beta\.[1-9][0-9]*)?$' || {
    echo "invalid release version: $1" >&2
    return 1
  }
  case "$1" in
    *-beta.*) echo beta ;;
    *) echo stable ;;
  esac
}
