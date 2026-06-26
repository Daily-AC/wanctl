---
name: wanctl
description: Drive another device across the internet via the wanctl MCP server — run commands, transfer files, and read activity logs as TYPED tools (no Bash shell-quoting, no parsing). Use when the user asks to control / run / push / pull / inspect a remote device they have wanctl-enrolled (e.g. "在 home-pc 上跑…", "把文件传到 lab 机器", "control my windows box", "看那台机器的日志"), or mentions wanctl.
---

# wanctl — control a remote device via MCP tools

This is the **MCP variant** of the wanctl skill. Instead of shelling out to
`wanctl exec/push/pull/…` through Bash, the user has set you up to call typed
tools directly: `wanctl_peers`, `wanctl_exec`, `wanctl_push`, `wanctl_pull`,
`wanctl_logs`, `wanctl_id`, `wanctl_trust`, `wanctl_rules`. Prefer these over
Bash whenever the user wants you to act on a remote device. If the tools are
NOT visible in your tool list, the MCP server hasn't been registered — tell the
user to register it (see below) or fall back to the Bash skill at
<https://wanctl-relay.***REMOVED***.***REMOVED***.com/skills>.

> **How this SKILL is distributed.** The user installs it by saying to you:
> **「安装 https://wanctl-relay.***REMOVED***.***REMOVED***.com/skills/mcp」**. When you see
> that, WebFetch `https://wanctl-relay.***REMOVED***.***REMOVED***.com/skills/mcp`, save the
> returned markdown to `~/.claude/skills/wanctl/SKILL.md` (creating the dir),
> and remind the user to (1) register the MCP server with their host, and
> (2) restart their host so both the skill and the MCP tools load.

## One-time setup (the user runs this once)

1. **Install the wanctl binary** (if not already):
   ```bash
   curl -fsSL https://wanctl-relay.***REMOVED***.***REMOVED***.com/install.sh | sh
   ```

2. **Get a token** by logging into the team portal:
   ```bash
   wanctl login    # opens a browser → Feishu SSO → paste back the one-time code
   ```

3. **Register the MCP server** with their AI host. For Claude Code:
   ```bash
   claude mcp add wanctl wanctl mcp
   ```
   For Cursor / Continue / other hosts, add this to their MCP config (JSON):
   ```json
   {
     "mcpServers": {
       "wanctl": {
         "command": "wanctl",
         "args": ["mcp"]
       }
     }
   }
   ```

4. Restart the host. The 8 `wanctl_*` tools should appear in your tool list.

## The 8 tools

- **`wanctl_peers`** — list devices online for the active token. Call this
  first when the user asks "what's reachable" or before guessing a target.
- **`wanctl_exec(target, command, cwd?, oneshot?)`** — run a shell command on
  `target`. Returns exit code + stdout + stderr in a single text block. cwd is
  also the policy scope for that command on the device. Successive calls share
  cwd/env unless `oneshot: true`.
- **`wanctl_push(target, local, remote)`** — upload a file from THIS machine to
  the target.
- **`wanctl_pull(target, remote, local)`** — download from the target to THIS
  machine.
- **`wanctl_logs(target, type?, grep?, since?, limit?)`** — pull the device's
  JSONL activity log (`connect` / `exec` / `file` events with decision and exit
  code). Useful to inspect what happened and why a command was denied.
- **`wanctl_id`** — print this controller's fingerprint + config dir.
- **`wanctl_trust(which?)`** — list pinned devices (`servers`, default) or
  trusted controllers (`clients`; only meaningful if this host also runs as
  an agent).
- **`wanctl_rules`** — list local policy rules. Empty for controller-only hosts.

`target` is either `DEVICE` (your namespace) or `NS/DEVICE` for a shared
device. If exactly one device is online for your token, you can pass `""`.

## Reading results correctly

- `wanctl_exec` succeeds (no `isError`) regardless of the remote exit code —
  treat it like a local command. The exit code is in the result text
  ("`exit: 0`" / "`exit: 1`").
- **Denied by policy** (the device hasn't allowed this command yet): the tool
  succeeds but stderr contains "command denied by device policy" or
  "write/read denied". This is NOT a tool error — it means the human at the
  device must allow it. Do not retry blindly; explain to the user what needs
  approval.
- **Blocked on approval**: when a portal user is attending, your call may
  block ~60s while they approve/deny. Expected; a timeout returns denial.
- **Pairing required (the killer case)**: when this is the very first time
  your controller fingerprint dials this device, the tool returns
  `isError: true` with text starting `PAIRING REQUIRED.` and a URL like
  `https://wanctl.***REMOVED***.***REMOVED***.com/#pair?device=...&fp=...&label=...`.

  **DO this**: paste the URL verbatim into your reply and ask the user to open
  it, click 「信任并继续」, then retry your tool call. The URL is valid 5
  minutes; one click trusts you forever.

  **DON'T**: paraphrase the URL, shorten it, wrap it in markdown link syntax,
  or hide it in a code fence — the user needs to copy/click the raw URL.

## Auditing what happened

```jsonc
// wanctl_logs → each line is JSON like:
{"ts":"…","type":"exec","peer_fp":"SHA256:…","peer_name":"…","detail":"…",
 "decision":"approved","exit":0}
```

`decision` ∈ `bypass | pre-approved | approved | remembered:dir |
remembered:global | denied`. Content lives only on the device (E2E — the relay
never sees it).

## When a command keeps getting denied

The device owner can pre-authorize it so it stops prompting. Tell them to:
- On the device: `wanctl rules add --kind exec --pattern "git status"` (or a
  directory rule: `--kind write --pattern /srv/app`), or choose **Allow +
  remember** in the portal device console when approving.
- For trusted, isolated devices: `wanctl agent --mode bypass` (dangerous;
  everything is still logged).

You cannot approve on the owner's behalf from the controller side.

## Enrolling a new device

If the user wants a new machine to become controllable:
```bash
curl -fsSL https://wanctl-relay.***REMOVED***.***REMOVED***.com/install.sh | sh
wanctl                          # on that machine — opens browser for Feishu
                                # login, takes a code, starts agent in background
```
For non-interactive (CI / pre-issued token): `WANCTL_TOKEN=<tok> sh` on the
curl line. After enrollment, refresh `wanctl_peers` — the new device should
show up.

## Notes

- One Go binary plays controller, agent, relay, and MCP server. The MCP server
  re-uses the same token / config / transport as the CLI.
- To upgrade `wanctl` itself: `wanctl update`. It fetches the latest binary
  from the relay and atomically replaces the current one; if a background
  daemon is running it is restarted automatically.
- The Bash variant of this skill (for hosts without MCP) is at
  <https://wanctl-relay.***REMOVED***.***REMOVED***.com/skills>.
- Source / design: `~/projects/wanctl` (mainline branch `main`).
