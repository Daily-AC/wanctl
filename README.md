# wanctl

wanctl controls devices across the public internet from a terminal or an AI
agent's shell. It carries commands and files through an end-to-end-encrypted
relay, while each device keeps final authority through a local approval policy.

## Features

- End-to-end mutual TLS 1.3 with Ed25519 identities and explicit fingerprint pinning.
- Device-side policy for command, file, log, and elevated operations, including human approval on rule misses.
- Friend relationships and per-device sharing with bounded exec/read/write capabilities.
- GitHub OAuth login with first-user administration and invite-based admission.
- One Go binary for relay, portal, agent, controller, and MCP roles.
- Proxy-agnostic HTTP long-poll transport that works through ordinary reverse proxies; WebSocket remains optional.
- CLI and MCP surfaces designed for scripted and AI-agent-driven control.

## Quick start

For a persistent public deployment, follow [the self-hosting guide](docs/self-hosting.md).
It starts Postgres, the relay, and the portal with Docker Compose and covers
GitHub OAuth, HTTPS termination, admission, and device enrollment.

To try the encrypted relay path locally without Postgres or OAuth, run the
[local smoke test](docs/architecture.md#local-smoke-test-no-external-services).

### Supported platforms

Every release ships signed binaries for the whole matrix below; the one-line
installers (`install.sh`, `install.ps1`) detect the OS and CPU and fetch the
matching one, and `wanctl update` does the same from inside the binary.

| OS | 64-bit | 32-bit |
|---|---|---|
| Linux | amd64, arm64 | 386, arm (armv6/v7), mips, mipsle (softfloat, routers) |
| macOS | amd64, arm64 (Apple silicon) | — (macOS has run no 32-bit code since Catalina) |
| Windows | amd64, arm64 | 386 |
| Android | arm64, amd64 — binary + APK each | arm (armeabi-v7a), 386 — binary + APK each |

The list lives in `scripts/release-targets.sh`, which the release build, the
publisher's checks and CI's cross-compile gate all read.

## Architecture

```text
controller (you/agent) --+                            +-- device (wanctl agent)
   wanctl exec/push/logs  |   relay (public broker)   |     policy engine + approval
                          +-- byte-pipe + registry ---+     JSONL event log
                          |   token auth + ACL + audit|
   E2E mutual-TLS ========+======== over the pipe ====+==== (relay sees only ciphertext)
                          |                           |
   portal (web, SSO) -----+  issues tokens, ACL -----+  (thin proxy to relay /admin/*)
```

The relay authenticates tokens and authorizes connections but cannot decrypt a
session. Controllers and devices establish mutual TLS over the relayed byte
pipe, then devices independently enforce local policy. The portal has no
database of its own; it authenticates users and scopes calls to the relay's
Postgres-backed admin API. See [Architecture](docs/architecture.md) for the
trust model, transports, sharing rules, and component map.

## Build

wanctl requires the Go release named by the `go` directive in `go.mod` (currently 1.26.6); the Go tool downloads it automatically.

```bash
go build -o wanctl .
go test ./...
```

The same binary selects its role by subcommand:

```bash
wanctl relay --addr :8080
wanctl portal --addr :8080
wanctl agent --relay https://relay.example.com --token TOKEN --name DEVICE
wanctl peers
wanctl exec --target DEVICE "uname -a"
```

## Documentation

- [Self-hosting](docs/self-hosting.md)
- [Environment variables](docs/environment.md)
- [Architecture and security model](docs/architecture.md)
- [Android](docs/android.md)
- [Release signing](docs/release-signing.md)
- [Security audit](docs/security-audit-2026-07-23.md)

## License

Licensed under the [Apache License 2.0](LICENSE). See [NOTICE](NOTICE) for
attribution information.
