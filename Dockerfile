# Multi-arch OCI index digests verified against Docker Hub on 2026-07-23.
FROM golang:1.26.6-alpine3.24@sha256:af8d6740070b8906d12eae1c3e3ea0957fb63f492051ea05e354c38ef9fe88df AS build
WORKDIR /src
COPY go.mod go.sum ./
# Overridable so builders behind a slow or blocked route to proxy.golang.org can
# point at a reachable mirror; unset, this is exactly Go's own default.
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
RUN go mod download
COPY . .
ARG WANCTL_VERSION=dev
ARG WANCTL_RELEASE_PUBLIC_KEYS=
RUN CGO_ENABLED=0 go build -tags lark -trimpath \
      -ldflags "-X main.buildVersion=${WANCTL_VERSION} -X wanctl/internal/release.TrustedPublicKeys=${WANCTL_RELEASE_PUBLIC_KEYS}" \
      -o /out/wanctl .

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
RUN addgroup -S -g 10001 wanctl && \
    adduser -S -D -H -u 10001 -G wanctl wanctl && \
    mkdir -p /data /dist && chown wanctl:wanctl /data /dist
COPY --from=build --chown=wanctl:wanctl /out/wanctl /usr/local/bin/wanctl
EXPOSE 8080
# Role is chosen at runtime: WANCTL_ROLE=relay (default) | portal | mcp.
# Same image serves all three thunderbox apps (relay = public + DB; portal =
# internal SSO; mcp = public HTTP/SSE MCP server). For "mcp" we invoke the
# subcommand with --http so it serves Streamable HTTP transport on :8080.
ENV WANCTL_ROLE=relay
# Mount the signed release/ directory produced by scripts/build-release.sh.
# Without a valid signed manifest, /dl/* deliberately returns 503.
ENV WANCTL_DIST_DIR=/dist
ENV WANCTL_CONFIG_DIR=/data
USER wanctl
WORKDIR /data
CMD ["sh", "-ec", "case \"$WANCTL_ROLE\" in relay|portal) exec wanctl \"$WANCTL_ROLE\" --addr :8080 ;; mcp) exec wanctl mcp --http :8080 ;; *) echo \"invalid WANCTL_ROLE: $WANCTL_ROLE\" >&2; exit 64 ;; esac"]
