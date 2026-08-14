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

proxy_is_loopback() {
  for proxy in "${HTTP_PROXY:-}" "${HTTPS_PROXY:-}" "${http_proxy:-}" "${https_proxy:-}"; do
    case "$proxy" in
      http://127.0.0.1:*|https://127.0.0.1:*|http://localhost:*|https://localhost:*) return 0 ;;
    esac
  done
  return 1
}

build_image() {
  need_docker
  mkdir -p "$artifacts"
  docker build --pull ${GOPROXY:+--build-arg GOPROXY="$GOPROXY"} -t "$image_name" "$root"
  docker save "$image_name" -o "$artifacts/wanctl-image.tar"
}

run_govulncheck() {
  cd "$root"
  # Pinned here as well as in .gitlab-ci.yml, and both have to move together:
  # this line wins inside the job, so bumping only the CI variable leaves the
  # scan reporting vulnerabilities in a toolchain the pipeline thinks it left
  # behind (measured 2026-08-14, pipeline 9859).
  GOTOOLCHAIN=go1.25.13 go run "golang.org/x/vuln/cmd/govulncheck@$govuln_version" ./...
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
  if proxy_is_loopback; then
    set -- --network host
  else
    set --
  fi
  # Trivy's DB unpacks to ~1GB, which does not fit in a tmpfs sized at half of
  # RAM on a small host. Keep the cache on disk; it also survives between runs.
  trivy_cache="$artifacts/trivy-cache"
  mkdir -p "$trivy_cache"
  docker run --rm --user "$(id -u):$(id -g)" --tmpfs /tmp:rw,noexec,nosuid,nodev,mode=1777 \
    "$@" \
    -e HTTP_PROXY -e HTTPS_PROXY -e NO_PROXY \
    -e http_proxy -e https_proxy -e no_proxy \
    -v "$trivy_cache:/trivy-cache" \
    -e TRIVY_CACHE_DIR=/trivy-cache \
    ${TRIVY_DB_REPOSITORY:+-e TRIVY_DB_REPOSITORY="$TRIVY_DB_REPOSITORY"} \
    ${TRIVY_JAVA_DB_REPOSITORY:+-e TRIVY_JAVA_DB_REPOSITORY="$TRIVY_JAVA_DB_REPOSITORY"} \
    -v "$artifacts:/scan:ro" "$trivy_image" \
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
