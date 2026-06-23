# Build the relay binary and run it. Used by thunderbox (build_pack=dockerfile).
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/wanctl .

FROM alpine:3.20
COPY --from=build /out/wanctl /usr/local/bin/wanctl
EXPOSE 8080
# Role is chosen at runtime: WANCTL_ROLE=relay (default) or portal. Same image
# serves both thunderbox apps (relay = public + DB; portal = internal SSO + DB).
ENV WANCTL_ROLE=relay
CMD ["sh", "-c", "wanctl ${WANCTL_ROLE} --addr :8080"]
