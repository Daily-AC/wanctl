# wanctl

Control a device **across the public internet** from your terminal (or an AI
agent's shell tool) over an **end-to-end encrypted, relayed** channel. The WAN
counterpart of [`lanctl`](../lanctl): instead of LAN multicast discovery, both
endpoints dial a relay deployed on thunderbox; the relay byte-pipes them and
**never sees plaintext** — the two ends run mutual TLS 1.3 over the pipe.

```
controller (you)          relay (public, thunderbox)        device (agent)
  wanctl exec/push ──ws──►  byte-pipe + registry  ◄──ws──  wanctl agent
       └──────── mutual-TLS E2E tunnel over the pipe ──────────┘
```

> **Status:** Foundation milestone (M1). Cross-internet transport, relay, device
> bind, and exec/file transfer all work end-to-end. The Claude-Code-style policy
> engine, local Web GUI, Feishu-SSO portal + Postgres + sharing ACL, and JSONL
> logging are later milestones — see `docs/superpowers/plans/`.

## Build

```bash
go build -o wanctl .                                  # this machine
GOOS=windows GOARCH=amd64 go build -o wanctl.exe .    # a Windows device
```

## Roles (one binary)

```bash
# Relay (on thunderbox; see docs/deploy.md):
WANCTL_TOKENS="tok:teamA" wanctl relay --addr :8080

# Device to be controlled:
wanctl agent --relay wss://wanctl-relay.***REMOVED***.***REMOVED***.com --token tok --name home-pc

# Controller:
export WANCTL_RELAY=wss://wanctl-relay.***REMOVED***.***REMOVED***.com WANCTL_TOKEN=tok
wanctl peers                         # list reachable devices
wanctl exec --target home-pc "..."   # run a command (streams output, real exit code)
wanctl push ./a.zip /remote/a.zip    # upload
wanctl pull /remote/log.txt ./log    # download
wanctl id                            # this node's fingerprint
wanctl trust [clients|servers]       # list trusted peers
```

First connection to a new device prints a TOFU pairing prompt on the device
(auto-trusted with `--yes` for unattended agents). Token leakage alone does not
grant control: the device still pins fingerprints and (later milestone) enforces
the policy engine.

## Security model

- **Token** (relay admission, machine-to-machine) → hashed/revocable later via the portal.
- **E2E mutual TLS 1.3** (relay sees only ciphertext) — Ed25519 identity reused from lanctl.
- **TOFU fingerprint pinning** both directions.

See `docs/superpowers/specs/2026-06-23-wanctl-design.md` for the full design.
