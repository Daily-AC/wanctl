# Multi-arch OCI index digests verified against Docker Hub on 2026-07-23.
FROM golang:1.25.12-alpine3.24@sha256:56961d79ea8129efddcc0b8643fd8a5416b4e6228cfd477e3fd61deb2672c587 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/wanctl .
# Prebuilt agent binaries served at /dl/<name> for the curl|sh installer.
RUN set -e; for t in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do \
      os=${t%/*}; arch=${t#*/}; \
      CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -ldflags "-s -w" -o /dist/wanctl-$os-$arch . ; \
    done; \
    CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "-s -w" -o /dist/wanctl-windows-amd64.exe .

FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
RUN addgroup -S -g 10001 wanctl && \
    adduser -S -D -H -u 10001 -G wanctl wanctl && \
    mkdir -p /data /dist && chown wanctl:wanctl /data /dist
COPY --from=build --chown=wanctl:wanctl /out/wanctl /usr/local/bin/wanctl
COPY --from=build --chown=wanctl:wanctl /dist /dist
EXPOSE 8080
# Role is chosen at runtime: WANCTL_ROLE=relay (default) | portal | mcp.
# Same image serves all three thunderbox apps (relay = public + DB; portal =
# internal SSO; mcp = public HTTP/SSE MCP server). For "mcp" we invoke the
# subcommand with --http so it serves Streamable HTTP transport on :8080.
ENV WANCTL_ROLE=relay
ENV WANCTL_DIST_DIR=/dist
ENV WANCTL_CONFIG_DIR=/data
USER wanctl
WORKDIR /data
CMD ["sh", "-ec", "case \"$WANCTL_ROLE\" in relay|portal) exec wanctl \"$WANCTL_ROLE\" --addr :8080 ;; mcp) exec wanctl mcp --http :8080 ;; *) echo \"invalid WANCTL_ROLE: $WANCTL_ROLE\" >&2; exit 64 ;; esac"]
