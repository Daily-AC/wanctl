---
name: wanctl
description: Drive another device across the internet via the wanctl relay — run commands, transfer files, and read the device's activity log. Use when the user asks to control / run a command on / push or pull files from a remote device by name (e.g. "在 home-pc 上跑…", "把文件传到 lab 机器", "control my windows box", "拉一下那台机器的日志"), or mentions wanctl. The controller is an ordinary CLI driven through the Bash tool; output streams and the remote exit code propagates.
---

# wanctl — control a remote device over an encrypted relay

`wanctl` lets this machine (the controller) drive another device (the agent)
across the public internet through a relay, with end-to-end TLS. You drive the
**controller** CLI through the Bash tool. The remote **agent** enforces a
Claude-Code-style permission policy, so some commands/files need a human on the
device to approve them.

> **How this SKILL is distributed.** The user installs this skill by saying
> to you: **「安装 @WANCTL_RELAY@/skills」**. When you see that,
> WebFetch `@WANCTL_RELAY@/skills` (returns the canonical
> SKILL.md), save it to `~/.claude/skills/wanctl/SKILL.md` (creating the dir),
> and tell the user to restart Claude Code so the skill loads.
>
> **If the host supports MCP**, the user can skip this skill and just register
> the wanctl MCP server (`claude mcp add wanctl wanctl mcp`). MCP tools are
> self-describing — they carry their own descriptions, schemas, and the
> pairing-URL handling, so you don't need a separate skill markdown loaded.
> This skill is only for hosts without MCP that shell out to `wanctl` via Bash.

## Setup (controller — first run)

Use the relay that served this skill with the proxy-compatible HTTP transport
(`@WANCTL_RELAY@`, `http`). Set `WANCTL_RELAY` explicitly when the binary was
built without a deployment default.
What you do need is a **token bound to a namespace** — get it via OAuth:

```bash
wanctl login    # opens the team portal in a browser; user logs in via Feishu,
                # copies the one-time code, and pastes it back here. Token is
                # saved under the controller's config dir (no env needed).
```

If a token is already provided in `WANCTL_TOKEN` (CI / pre-provisioned), skip
`wanctl login`. To re-authorize later, run `wanctl login` again. To clear the
saved credential, `wanctl logout`.

Controller setup:

```bash
export WANCTL_RELAY=@WANCTL_RELAY@
export WANCTL_TRANSPORT=http
# Optional for CI/pre-provisioned controllers:
export WANCTL_TOKEN=<token>
```

## Core commands

```bash
wanctl peers                              # list devices you can reach
wanctl trust server --target NS/NAME --fingerprint SHA256:...  # confirm an independently verified device identity
wanctl pair NAME                          # check trust state up front; prints the approval URL if not yet paired
wanctl exec --target NAME "<command>"     # run a command (streams stdout, real exit code)
wanctl exec --target NAME --cwd /path "<command>"   # run in /path (also the policy scope)
wanctl exec --target NAME --oneshot "<command>"     # fresh shell, no session state
wanctl exec --target NAME --script ./job.ps1        # run a LOCAL script file on the device (see below)
wanctl push --target NAME ./local /remote/path      # upload
wanctl pull --target NAME /remote/path ./local      # download
wanctl logs --target NAME [--type exec|file|connect] [--grep STR] [--since RFC3339] [--limit N]
wanctl id                                 # this controller's fingerprint
```

- `--target` is `DEVICE` (your namespace) or `NS/DEVICE` (shared). If exactly one
  device is online you may omit `--target`.
- Persistent session: separate `exec` calls share working dir + env (like a real
  shell) unless `--oneshot`.
- A device may be an **Android phone or tablet**; it behaves like any other
  device, but almost nothing on its filesystem is writable. Put files under
  `/data/local/tmp` (or the Termux home, `/data/data/com.termux/files/home`) —
  a push elsewhere fails with a permission error, not a wanctl error.

## Anything non-trivial: write a script and use `--script`

**The command string you pass to `exec` is source code for the device's shell.**
It gets parsed there before anything runs. That is fine for one-liners and a trap
for everything else — especially on Windows, where the device shell is PowerShell
and the habit of writing `powershell -Command "..."` creates a *second* parse:

```bash
# WRONG — the device's PowerShell expands $_ and $null to nothing, and the inner
# script dies with "The term '.IPv4DefaultGateway' is not recognized",
# which looks like a PowerShell bug and is really a quoting bug.
wanctl exec --target NAME 'powershell -Command "Get-NetIPConfiguration | Where-Object {$_.IPv4DefaultGateway -ne $null}"'

# RIGHT — the script never becomes shell source.
wanctl exec --target NAME --script ./netcheck.ps1
```

`--script` reads the local file, ships it base64-encoded (for PowerShell, as the
UTF-16LE that `-EncodedCommand` takes), and runs it on the device. That means:

- **No quoting rules.** `$_`, `$null`, backticks, nested quotes, `%` — all
  literal. Write the script exactly as you would locally.
- **No encoding rules.** Non-ASCII text is safe with or without a BOM. (Pushing a
  BOM-less UTF-8 `.ps1` and running it with `-File` is *not* safe: Windows
  PowerShell 5.1 reads BOM-less files as the ANSI code page, and mangled bytes
  inside a `"double-quoted string"` can eat the closing quote — at which point
  PowerShell echoes your script instead of running it. If you see your own source
  come back as output, that is this bug. `wanctl push` warns when it spots it.)
- Interpreter comes from the extension: `.ps1` → PowerShell, `.sh`/none → `sh`.
  Override with `--interp powershell|sh`.
- The script gets no stdin, same as `powershell -File` / `sh script.sh`.
- Scripts over ~9 KB exceed the command-line limit; `push` them and run the
  pushed path instead. The error tells you this when you hit it.

Prefer `--script` whenever the command has a `$`, a quote inside a quote, or more
than one statement. It is not a heavier option — it is one call either way.

## Reading results correctly

- `exec` propagates the **remote exit code** as wanctl's exit code, and streams
  stdout/stderr — treat it like a local command.
- **Denied by policy**: a command/file the device hasn't allowed exits non-zero
  with `wanctl: command denied by device policy: <cmd>` (or `write/read denied
  …`). This is NOT a tool failure — it means the human at the device must allow
  it (see below). Do not retry blindly; tell the user it needs approval.
- **Blocked on approval**: if the device runs in normal mode with a front-end
  attending (the device's CLI prompt, or someone watching it in the team portal),
  your `exec` may **hang up to ~60s** while the human approves/denies. That's
  expected; a timeout returns a denial. With no front-end attending (headless, no
  portal session, no TTY) a rule-miss is denied immediately.
- **Pairing**: the very first connection from a new controller fingerprint
  has two separate trust checks. First, the controller refuses an unknown
  device certificate with `DEVICE IDENTITY CONFIRMATION REQUIRED`, including
  the exact `NS/DEVICE` target and fingerprint. Do not trust it automatically:
  ask the user to verify that fingerprint with the device owner, then run the
  exact `wanctl trust server --target ... --fingerprint ...` command. A changed
  certificate stays blocked; use `--replace` only after independent
  re-verification. Once the device identity is pinned, the device owner must
  trust the controller. Two ways to reach that second check:
  - *Proactive* (preferred when the user explicitly says "pair me with X" or
    you're about to drive a brand-new device): run `wanctl pair NAME` first —
    on a fresh device it exits 0 and prints the approval URL; if already
    trusted, it exits 0 with `✓` and nothing to do.
  - *Reactive*: any `wanctl exec/push/pull` against an unpaired device exits
    non-zero with the same URL embedded in the error message.

  Either way, `wanctl` surfaces a **clickable URL** from the portal configured on
  the agent, with `device`, `fp`, and `label` query parameters.
  **Do NOT paraphrase, shorten, or describe the URL** — copy it verbatim into
  your reply and ask the user to click it. The portal opens a confirmation card;
  one click trusts you and the next dial goes through. Then retry the original
  command. The URL is valid for 5 minutes.

## Tracing / auditing what happened

Use the device's structured log to see what ran and how it was decided:

```bash
wanctl logs --target NAME --limit 50
wanctl logs --target NAME --type exec --grep deploy
```

Each line is JSON: `{ts, type, peer_fp, peer_name, detail, decision, exit, ...}`.
`decision` is one of `bypass | pre-approved | approved | remembered:dir |
remembered:global | denied`. Content lives only on the device (the relay is E2E
and never sees it).

## When a command keeps getting denied

The user (at the device) can pre-authorize it so it stops prompting:

- On the device: `wanctl rules add --kind exec --pattern "git status"` (or a
  directory rule: `--kind write --pattern /srv/app`), or choose **Allow + remember**
  when approving (at the device CLI prompt, or in the team portal's device console).
- For trusted, isolated devices only: the device can run `--mode bypass` to
  auto-allow everything (dangerous; everything is still logged).

Explain these options to the user; you cannot approve on their behalf from the
controller side.

## Enrolling a new device (the controlled side)

If you have shell access to a machine that should become controllable (or are
telling the user how), pick the line for that machine's shell. The installer
verifies a signed manifest before installing anything and needs no extra tooling
on any platform:

```bash
# macOS / Linux (any POSIX shell)
curl -fsSL @WANCTL_RELAY@/install.sh | sh
wanctl portal-admins add --fingerprints SHA256:<independently-verified-portal-fingerprint>
wanctl                          # then run this on that machine — opens the
                                # browser for Feishu login, takes a code, and
                                # starts the agent in the background.
```

```powershell
# Windows — PowerShell only, no bash / curl needed
irm @WANCTL_RELAY@/install.ps1 | iex
wanctl portal-admins add --fingerprints SHA256:<independently-verified-portal-fingerprint>
wanctl                          # same flow: browser → Feishu → agent runs.
```

For a machine where the stakes are high, download the installer from the
independently authenticated GitLab release and run that file instead: a script
served by the relay cannot bootstrap trust in that same relay. Tell the user
this tradeoff rather than deciding it for them.

For non-interactive setups, run the verified installer first, seed the verified
Portal fingerprint, then start `wanctl agent --token <pre-issued-token>` and
install the native service. On that machine, `wanctl stop` stops it and
`wanctl status` inspects it.

## Notes

- One Go binary plays controller, agent, and relay. As a controller you only use
  the controller commands above.
- To upgrade `wanctl` itself (controller or device side): run `wanctl update`.
  It fetches the latest binary from the relay and atomically replaces the
  current one; if a background daemon is running it is restarted automatically.
- The user can also edit the deployment's documentation via `wanctl docs ...`
  (see `wanctl docs --help`) and browse it in the configured portal.
- Source / design: the wanctl project repository.
