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

## Setup (controller env)

These must be set (ask the user if missing):

```bash
export WANCTL_RELAY=wss://wanctl-relay.***REMOVED***.***REMOVED***.com   # or https:// for http transport
export WANCTL_TOKEN=<the user's access token>
export WANCTL_TRANSPORT=http     # REQUIRED for the thunderbox relay (its nginx
                                 # strips WebSocket upgrades). Use http + an
                                 # https:// relay URL. Omit only if the relay is
                                 # known to support WebSocket.
```

With `WANCTL_TRANSPORT=http`, set `WANCTL_RELAY` to the `https://` form.

## Core commands

```bash
wanctl peers                              # list devices you can reach
wanctl exec --target NAME "<command>"     # run a command (streams stdout, real exit code)
wanctl exec --target NAME --cwd /path "<command>"   # run in /path (also the policy scope)
wanctl exec --target NAME --oneshot "<command>"     # fresh shell, no session state
wanctl push --target NAME ./local /remote/path      # upload
wanctl pull --target NAME /remote/path ./local      # download
wanctl logs --target NAME [--type exec|file|connect] [--grep STR] [--since RFC3339] [--limit N]
wanctl id                                 # this controller's fingerprint
```

- `--target` is `DEVICE` (your namespace) or `NS/DEVICE` (shared). If exactly one
  device is online you may omit `--target`.
- Persistent session: separate `exec` calls share working dir + env (like a real
  shell) unless `--oneshot`.

## Reading results correctly

- `exec` propagates the **remote exit code** as wanctl's exit code, and streams
  stdout/stderr — treat it like a local command.
- **Denied by policy**: a command/file the device hasn't allowed exits non-zero
  with `wanctl: command denied by device policy: <cmd>` (or `write/read denied
  …`). This is NOT a tool failure — it means the human at the device must allow
  it (see below). Do not retry blindly; tell the user it needs approval.
- **Blocked on approval**: if the device runs in normal mode with a human
  present (console or web GUI), your `exec` may **hang up to ~60s** while the
  human clicks allow/deny. That's expected. If it times out it returns a denial.
- **Pairing**: the very first connection from a new controller fingerprint must
  be approved once on the device (TOFU). Until then you get a reject telling you
  to approve it on the device.

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
  directory rule: `--kind write --pattern /srv/app`), or click **Allow + remember**
  in the device web console (`wanctl agent --gui-port 7600`).
- For trusted, isolated devices only: the device can run `--mode bypass` to
  auto-allow everything (dangerous; everything is still logged).

Explain these options to the user; you cannot approve on their behalf from the
controller side.

## Notes

- One Go binary plays controller, agent, and relay. As a controller you only use
  the commands above.
- Source / design: `~/projects/wanctl` (branch `http-transport`).
