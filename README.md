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

> **Status:** M1 (cross-internet transport, relay, exec/file) + M2 (policy &
> approval) done and verified over the public thunderbox relay. On `http-transport`
> the proxy-agnostic HTTP transport works through thunderbox's nginx (which strips
> WS upgrades). Remaining: local Web GUI (M3), Feishu-SSO portal + Postgres +
> sharing ACL (M4), JSONL logging (M5), the skill (M6) — see `docs/superpowers/plans/`.

## Permissions (Claude-Code style)

Every remote command and file op is gated on the device:
- pre-approved by a rule → runs; otherwise the device prompts `y` (once) / `a`
  (remember this dir) / `g` (remember global) / `n` (deny).
- `wanctl agent --mode bypass` auto-allows everything (warns; for trusted/isolated devices).
- Headless agents deny on a miss — pre-load rules with `wanctl rules add`.
- Manage rules on the device: `wanctl rules list|add|rm`.
- `wanctl exec --cwd /path "cmd"` runs in (and scopes the rule to) that directory.

### Local web console (M3)

`wanctl agent --gui-port 7600` opens a localhost UI (`http://127.0.0.1:7600`) for
the human at the device: live approval queue (click `y/a/g/n`), rule management,
a mode toggle (with a bypass danger banner), and an activity timeline. When
`--gui-port` is set, the browser is the approver.

### Activity log (M5)

The device records every connect/exec/file action to `<config>/logs/events.jsonl`
with the decision and exit code. Query it:

```bash
wanctl logs --target home-pc --type exec --grep deploy --since 2026-06-23T00:00:00Z
wanctl logs                      # run on the device itself: read the local log
```
Content stays on the device (E2E — the relay never sees it).

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
# Behind a reverse proxy that strips WebSocket upgrades (e.g. thunderbox nginx),
# use the proxy-agnostic HTTP transport instead:
wanctl agent --relay https://wanctl-relay.***REMOVED***.***REMOVED***.com --token tok --name home-pc --transport http
# ...and on the controller: export WANCTL_TRANSPORT=http

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

## Driving it from an agent (the skill)

`skill/SKILL.md` is a Claude Code skill that teaches an agent to drive the
controller CLI: setup env, run/transfer/log commands, and correctly read
"denied by policy" / "blocked on approval" / TOFU-pairing outcomes. Install by
copying `skill/` to `~/.claude/skills/wanctl/`.

See `docs/superpowers/specs/2026-06-23-wanctl-design.md` for the full design.
