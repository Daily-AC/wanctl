#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
artifacts=${WANCTL_ARTIFACTS_DIR:-"$root/artifacts"}
image_name=${WANCTL_SCAN_IMAGE:-wanctl:security-scan}
govuln_version=v1.6.0
syft_image=anchore/syft:v1.49.0@sha256:13b53ebabe3d215268c90cf8fb9b875f0183908245f376fd4b3a2cb69d21d484
trivy_image=aquasec/trivy:0.72.0@sha256:cffe3f5161a47a6823fbd23d985795b3ed72a4c806da4c4df16266c02accdd6f

need_docker() {
  command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 1; }
  docker info >/dev/null 2>&1 || { echo "docker daemon is unavailable" >&2; exit 1; }
}

container_proxy_url() {
  case "$1" in
    http://127.0.0.1:*) printf 'http://host.docker.internal:%s' "${1#http://127.0.0.1:}" ;;
    https://127.0.0.1:*) printf 'https://host.docker.internal:%s' "${1#https://127.0.0.1:}" ;;
    http://localhost:*) printf 'http://host.docker.internal:%s' "${1#http://localhost:}" ;;
    https://localhost:*) printf 'https://host.docker.internal:%s' "${1#https://localhost:}" ;;
    *) printf '%s' "$1" ;;
  esac
}

build_image() {
  need_docker
  mkdir -p "$artifacts"
  docker build --pull -t "$image_name" "$root"
  docker save "$image_name" -o "$artifacts/wanctl-image.tar"
}

run_govulncheck() {
  cd "$root"
  GOTOOLCHAIN=go1.25.12 go run "golang.org/x/vuln/cmd/govulncheck@$govuln_version" ./...
}

run_sbom() {
  need_docker
  [ -f "$artifacts/wanctl-image.tar" ] || build_image
  docker run --rm --user "$(id -u):$(id -g)" --tmpfs /tmp:rw,noexec,nosuid,nodev,mode=1777 \
    -e XDG_CACHE_HOME=/tmp/syft-cache -v "$artifacts:/out" "$syft_image" \
    docker-archive:/out/wanctl-image.tar -o cyclonedx-json=/out/wanctl.sbom.cdx.json
}

run_image_scan() {
  need_docker
  [ -f "$artifacts/wanctl-image.tar" ] || build_image
  HTTP_PROXY=$(container_proxy_url "${HTTP_PROXY:-}") \
    HTTPS_PROXY=$(container_proxy_url "${HTTPS_PROXY:-}") \
    http_proxy=$(container_proxy_url "${http_proxy:-}") \
    https_proxy=$(container_proxy_url "${https_proxy:-}") \
    docker run --rm --user "$(id -u):$(id -g)" --tmpfs /tmp:rw,noexec,nosuid,nodev,mode=1777 \
    --add-host host.docker.internal:host-gateway \
    -e HTTP_PROXY -e HTTPS_PROXY -e NO_PROXY \
    -e http_proxy -e https_proxy -e no_proxy \
    -e TRIVY_CACHE_DIR=/tmp/trivy-cache -v "$artifacts:/scan:ro" "$trivy_image" \
    image --input /scan/wanctl-image.tar --scanners vuln --severity HIGH,CRITICAL --ignore-unfixed --exit-code 1
}

run_smoke() {
  need_docker
  docker image inspect "$image_name" >/dev/null 2>&1 || build_image
  "$root/scripts/container-smoke.sh"
}

case "${1:-all}" in
  govulncheck) run_govulncheck ;;
  build) build_image ;;
  smoke) run_smoke ;;
  sbom) run_sbom ;;
  image) run_image_scan ;;
  all) run_govulncheck; build_image; run_smoke; run_sbom; run_image_scan ;;
  *) echo "usage: $0 [govulncheck|build|smoke|sbom|image|all]" >&2; exit 64 ;;
esac
