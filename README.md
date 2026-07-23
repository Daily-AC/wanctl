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

Admission tokens are sent in the `Authorization: Bearer` header and must be
protected by TLS. The E2E tunnel protects command/file payloads from the relay,
but it does not encrypt the outer HTTP/WebSocket admission handshake. In
particular, a `ws://` LAN relay exposes bearer headers to the LAN unless the
network path is already protected by a trusted encrypted tunnel. Use `wss://`
for production LAN relays whenever certificates are available.

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
- Exec rules match additional arguments only within one simple command; shell
  operators, substitutions, and redirections require an exact rule.
- File rules are enforced by root-scoped opens; symlinks cannot escape an
  allowed directory, and transfers reject non-regular files.
- Uploads are limited to 1 GiB and atomically replace the destination only
  after the declared byte count is received and synced successfully.
- `wanctl exec --cwd /path "cmd"` runs in (and scopes the rule to) that directory.

### Device console (CLI + remote portal)

The device itself has no web UI. A human at the device approves requests at the
CLI (`y/a/g/n`) when running in an interactive terminal. The single web console
lives in the **team portal**: open a device there to drive its live approval
queue, rule management, mode toggle (with a bypass danger banner), and activity —
remotely over the E2E tunnel. With no front-end attending (headless, no portal
session, no TTY), a rule-miss is denied immediately.

### Activity log (M5)

The device records connect/exec/file/log-read actions to
`<config>/logs/events.jsonl` with the decision and exit code. Secret-shaped
arguments are redacted before JSON encoding, and the log rotates at 1 MiB with
three backups. Query it:

```bash
wanctl logs --target home-pc --type exec --grep deploy --since 2026-06-23T00:00:00Z
wanctl logs                      # run on the device itself: read the local log
```
Content stays on the device (E2E — the relay never sees it).
Remote reads use a separate `logs` policy capability; approve the request once,
remember it globally, or pre-authorize it with `wanctl rules add --kind logs`.

HTTP tunnel writes and background jobs are bounded: one tunnel request is at
most 1 MiB; at most four background jobs run concurrently for up to 30 minutes,
with 8 MiB output retained per job and 64 MiB across completed jobs.

## Enroll a device in one line

On the machine you want to control (the agent), no Go needed — the relay serves
prebuilt binaries and a one-line installer for both shells. Get a token from the
portal, then:

```bash
# macOS / Linux
curl -fsSL https://***REMOVED-IP***/install.sh | WANCTL_TOKEN=<token> sh
```

```powershell
# Windows (PowerShell — no bash needed)
$env:WANCTL_TOKEN='<token>'; irm https://***REMOVED-IP***/install.ps1 | iex
```

Both installers detect OS/arch, install `wanctl`, and start the agent in the
foreground (wrap in `systemd`/`nohup &` on unix, or a service wrapper like
`nssm` on Windows, to persist). Optional env (same names on both):
`WANCTL_NAME` (default hostname), `WANCTL_MODE=bypass`, `WANCTL_INSTALL_ONLY=1`
(install, don't run), `WANCTL_BIN` (override install path). Install-only +
launch separately:

```bash
curl -fsSL https://***REMOVED-IP***/install.sh | WANCTL_INSTALL_ONLY=1 sh
nohup wanctl agent --relay https://***REMOVED-IP*** --token <token> \
      --transport ws --name "$(hostname)" >/tmp/wanctl-agent.log 2>&1 &
```

```powershell
$env:WANCTL_INSTALL_ONLY='1'; irm https://***REMOVED-IP***/install.ps1 | iex
wanctl agent --relay https://***REMOVED-IP*** --token <token> `
             --transport ws --name $env:COMPUTERNAME
```

> **For AI agents:** how to *drive* a device (run commands, transfer files, read
> logs, and interpret approval/denial) is in
> [`internal/portal/skill.md`](internal/portal/skill.md) — read it first.
> The portal serves the canonical copy at https://***REMOVED-IP***/skills ,
> so users install it by simply saying to their AI:
> **「安装 https://***REMOVED-IP***/skills」**
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
wanctl agent --relay https://***REMOVED-IP*** --token tok --name home-pc
# Behind a reverse proxy that strips WebSocket upgrades,
# use the proxy-agnostic HTTP transport instead:
wanctl agent --relay https://***REMOVED-IP*** --token tok --name home-pc --transport http
# ...and on the controller: export WANCTL_TRANSPORT=http

# Controller:
export WANCTL_RELAY=https://***REMOVED-IP*** WANCTL_TOKEN=tok
wanctl peers                         # list reachable devices
wanctl exec --target home-pc "..."   # run a command (streams output, real exit code)
wanctl push ./a.zip /remote/a.zip    # upload
wanctl pull /remote/log.txt ./log    # download
wanctl id                            # this node's fingerprint
wanctl trust [clients|servers]       # list trusted peers
wanctl trust server --target alice/home-pc --fingerprint SHA256:... # confirm a verified device identity
```

First contact with a new device stops before application data is sent and prints
its full `owner/device` target and certificate fingerprint. Verify both out of
band, then pin them with `wanctl trust server`; a changed certificate remains a
hard error unless explicitly re-pinned with `--replace`. After that, a new
controller still needs device-side approval (auto-trusted with `--yes` for
unattended agents). Token leakage alone therefore does not grant control.

## Security model

- **Token** (relay admission, machine-to-machine) → hashed/revocable later via the portal.
- **E2E mutual TLS 1.3** (relay sees only ciphertext) — Ed25519 identity reused from lanctl.
- **Explicit server fingerprint pinning** on controllers; device-side approval for new controllers.

## Driving it from an agent (the skill)

`internal/portal/skill.md` is a Claude Code skill that teaches an agent to drive
the controller CLI: setup env, run/transfer/log commands, and correctly read
"denied by policy" / "blocked on approval" / TOFU-pairing outcomes. The portal
serves the canonical copy at <https://***REMOVED-IP***/skills>. Users
install it by saying to their AI:

> 安装 https://***REMOVED-IP***/skills

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
