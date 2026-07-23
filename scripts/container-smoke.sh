#!/bin/sh
set -eu

image_name=${WANCTL_SCAN_IMAGE:-wanctl:security-scan}
containers=""

cleanup() {
  for container in $containers; do
    docker rm -f "$container" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT INT TERM

uid=$(docker run --rm --entrypoint id "$image_name" -u)
[ "$uid" = 10001 ] || { echo "container UID is $uid, want 10001" >&2; exit 1; }
docker run --rm --entrypoint sh "$image_name" -ec 'test -w /data && test -w /dist'

start_role() {
  role=$1
  shift
  container=$(docker run -d --rm -p 127.0.0.1::8080 -e "WANCTL_ROLE=$role" "$@" "$image_name")
  containers="$containers $container"
  port=$(docker port "$container" 8080/tcp | sed -n 's/.*://p' | head -1)
  [ -n "$port" ] || { echo "no published port for $role" >&2; exit 1; }

  attempt=0
  while [ "$attempt" -lt 30 ]; do
    case "$role" in
      relay) curl -fsS "http://127.0.0.1:$port/healthz" 2>/dev/null | grep -qx ok && return ;;
      portal) curl -fsS -o /dev/null "http://127.0.0.1:$port/" 2>/dev/null && return ;;
      mcp) curl --max-time 2 -sS -o /dev/null -H 'Content-Type: application/json' -d '{}' "http://127.0.0.1:$port/mcp" 2>/dev/null && return ;;
    esac
    attempt=$((attempt + 1))
    sleep 1
  done
  docker logs "$container" >&2 || true
  echo "$role role failed its HTTP smoke test" >&2
  exit 1
}

start_role relay -e WANCTL_TOKENS=smoke:team
start_role portal
start_role mcp -e WANCTL_MCP_SEED=000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f

echo "container smoke passed for relay, portal, and mcp as UID 10001"
