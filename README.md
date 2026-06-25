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
> approval) + M4 (Feishu-SSO portal + Postgres + sharing ACL) + M5 (JSONL logging)
> + M6 (skill) done and verified over the public thunderbox relay. The
> proxy-agnostic HTTP transport works through thunderbox's nginx (which strips WS
> upgrades). The device console (approvals/rules/mode/activity) is driven from the
> team portal over the E2E tunnel — there is no device-local web UI. See
> `docs/superpowers/plans/`.

## Permissions (Claude-Code style)

Every remote command and file op is gated on the device:
- pre-approved by a rule → runs; otherwise the device prompts `y` (once) / `a`
  (remember this dir) / `g` (remember global) / `n` (deny).
- `wanctl agent --mode bypass` auto-allows everything (warns; for trusted/isolated devices).
- Headless agents deny on a miss — pre-load rules with `wanctl rules add`.
- Manage rules on the device: `wanctl rules list|add|rm`.
- `wanctl exec --cwd /path "cmd"` runs in (and scopes the rule to) that directory.

### Device console (CLI + remote portal)

The device itself has no web UI. A human at the device approves requests at the
CLI (`y/a/g/n`) when running in an interactive terminal. The single web console
lives in the **team portal**: open a device there to drive its live approval
queue, rule management, mode toggle (with a bypass danger banner), and activity —
remotely over the E2E tunnel. With no front-end attending (headless, no portal
session, no TTY), a rule-miss is denied immediately.

### Activity log (M5)

The device records every connect/exec/file action to `<config>/logs/events.jsonl`
with the decision and exit code. Query it:

```bash
wanctl logs --target home-pc --type exec --grep deploy --since 2026-06-23T00:00:00Z
wanctl logs                      # run on the device itself: read the local log
```
Content stays on the device (E2E — the relay never sees it).

## Enroll a device in one line (curl | sh)

On the machine you want to control (the agent), no Go needed — the relay serves a
prebuilt binary and an installer. Get a token from the portal, then:

```bash
curl -fsSL https://wanctl-relay.***REMOVED***.***REMOVED***.com/install.sh | WANCTL_TOKEN=<token> sh
```

It detects OS/arch, installs `wanctl`, and runs the agent (foreground; wrap in
`systemd`/`nohup &` to persist). Optional env: `WANCTL_NAME` (default hostname),
`WANCTL_MODE=bypass`, `WANCTL_INSTALL_ONLY=1` (install, don't run). Run as a
background service:

```bash
curl -fsSL https://wanctl-relay.***REMOVED***.***REMOVED***.com/install.sh | WANCTL_INSTALL_ONLY=1 sh
nohup wanctl agent --relay https://wanctl-relay.***REMOVED***.***REMOVED***.com --token <token> \
      --transport http --name "$(hostname)" >/tmp/wanctl-agent.log 2>&1 &
```

> **For AI agents:** how to *drive* a device (run commands, transfer files, read
> logs, and interpret approval/denial) is in
> [`internal/portal/skill.md`](internal/portal/skill.md) — read it first.
> The portal serves the canonical copy at https://wanctl-relay.***REMOVED***.***REMOVED***.com/skills ,
> so users install it by simply saying to their AI:
> **「安装 https://wanctl-relay.***REMOVED***.***REMOVED***.com/skills」**
> (the agent WebFetches that URL and writes it to `~/.claude/skills/wanctl/SKILL.md`).

## Self-update

```bash
wanctl update                                  # fetch latest binary from the relay
                                               # and atomically swap it in; restarts daemon
```

## Build from source

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

`internal/portal/skill.md` is a Claude Code skill that teaches an agent to drive
the controller CLI: setup env, run/transfer/log commands, and correctly read
"denied by policy" / "blocked on approval" / TOFU-pairing outcomes. The portal
serves the canonical copy at <https://wanctl-relay.***REMOVED***.***REMOVED***.com/skills>. Users
install it by saying to their AI:

> 安装 https://wanctl-relay.***REMOVED***.***REMOVED***.com/skills

The agent fetches that URL and writes it to `~/.claude/skills/wanctl/SKILL.md`.

## Documentation

`https://wanctl.***REMOVED***.***REMOVED***.com` 的「使用文档」是一个由 Postgres 支撑的小博客
（不再硬编码在 SPA 里）。任何登录用户可在浏览器里编辑；CLI 通过命名空间 token
读写：

```bash
wanctl docs ls                                # 列文章
wanctl docs get enroll-device                 # 看正文
wanctl docs new --slug intro --title 'Intro' --group quickstart --editor
wanctl docs edit enroll-device --editor       # 改一篇
wanctl docs group new --slug ops --title '运维'
```

See `docs/superpowers/specs/2026-06-23-wanctl-design.md` for the full design.
