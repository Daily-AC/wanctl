# Build the relay binary and run it. Used by thunderbox (build_pack=dockerfile).
FROM golang:1.25-alpine AS build
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

FROM alpine:3.20
COPY --from=build /out/wanctl /usr/local/bin/wanctl
COPY --from=build /dist /dist
EXPOSE 8080
# Role is chosen at runtime: WANCTL_ROLE=relay (default) | portal | mcp.
# Same image serves all three thunderbox apps (relay = public + DB; portal =
# internal SSO; mcp = public HTTP/SSE MCP server). For "mcp" we invoke the
# subcommand with --http so it serves Streamable HTTP transport on :8080.
ENV WANCTL_ROLE=relay
ENV WANCTL_DIST_DIR=/dist
CMD ["sh", "-c", "if [ \"$WANCTL_ROLE\" = \"mcp\" ]; then exec wanctl mcp --http :8080; else exec wanctl ${WANCTL_ROLE} --addr :8080; fi"]
