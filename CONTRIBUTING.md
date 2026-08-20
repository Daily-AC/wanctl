# Contributing to wanctl

Thanks for considering a contribution. This page describes how changes get
built, checked, and reviewed.

## Development setup

wanctl is a single Go module. The only hard requirement is the Go toolchain
version pinned in `go.mod` (Go downloads it automatically when your installed
Go is newer than 1.21).

```sh
git clone https://github.com/Daily-AC/wanctl.git
cd wanctl
go build ./...
go test ./...
```

The Android app under `android/` is built by `scripts/build-apk.sh` and needs
the Android SDK; you do not need it for changes to the Go code.

## Checks that must pass

CI runs these on every push and pull request; running them locally first saves
a round trip:

```sh
test -z "$(gofmt -l .)"        # formatting gate: offenders fail the build
go test ./...
go vet ./...
go build -tags lark ./...      # Feishu notification path is build-tagged
go test -tags lark ./...
go vet -tags lark ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...
CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build ./...
```

The supply-chain job additionally runs `./scripts/security-scan.sh all`
(govulncheck, image scan, SBOM). It needs Docker; it is fine to leave that one
to CI.

## What a good change looks like

- One topic per pull request. Refactors and behavior changes travel separately.
- New behavior comes with a test that fails without the change. For anything
  touching the transport, policy, or trust code, prefer a test that models the
  real peer's behavior over a mock that mirrors the implementation.
- wanctl is a remote-control tool, so the security posture is conservative:
  anything that widens what a controller can do on a device, weakens a
  default, or adds a bypass needs an explicit justification in the PR
  description, and will get extra scrutiny.
- Comments explain constraints the code cannot show; match the density and
  tone of the file you are editing.

## Reporting bugs and proposing features

Use the issue templates. For anything that looks like a security problem, do
not open a public issue — see [SECURITY.md](SECURITY.md).
