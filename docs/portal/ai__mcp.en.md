# Connect an AI over MCP

MCP is how an AI calls wanctl directly — no skill to read, no command line to assemble. `wanctl_peers`, `wanctl_exec` and the rest simply appear in its tool list. The same 17 tools arrive two ways, and only one thing decides which: **can that AI start a `wanctl` process on its own machine?**

| | Use | Typically |
| --- | --- | --- |
| It can | Local stdio | Claude Code, Codex, Cursor |
| It cannot | Hosted endpoint | claude.ai on the web, cloud agent runners, an AI someone else hosts |

## Local (stdio)

One line for Claude Code:

```sh
claude mcp add wanctl -- wanctl mcp
```

For Codex, into `~/.codex/config.toml`:

```toml
[mcp_servers.wanctl]
command = "wanctl"
args = ["mcp"]
```

It uses the identity `wanctl login` already stored on that machine, so a restart is the whole setup and there is no second login. If the machine has never logged in, walk through [Let your AI control a device](#docs/ai-skill) first.

## Hosted (HTTP)

The endpoint is `https://relay.example.com/mcp`. In Claude Code:

```sh
claude mcp add --transport http wanctl https://relay.example.com/mcp
```

Any other host takes the same URL in its "MCP server / HTTP" field. It needs no file from your machine, and you never hand it a token. Some relays answer on `/wanctl-mcp` instead, because the proxy in front of them has claimed the `/mcp` prefix; ask whoever runs it which one to use.

> The endpoint is public and anyone can complete a handshake with it — and see no devices at all afterwards. What a session can see is decided entirely by the login below.

## Logging in the first time

Tell the AI to log in to wanctl. It calls `wanctl_login`, and you follow it:

1. The AI gives you a `https://portal.example.com/enroll` link. Open it in your browser.
2. The portal recognizes your account (usually you are already signed in) and shows a one-time code, good for five minutes.
3. Paste the code back to the AI. It calls `wanctl_login` again, this time with the code.

A login belongs to **one session**. Other people and other conversations on the same endpoint log in on their own, and none of them sees anyone else's devices.

The first time it then reaches a device, that device's **Waiting** page raises a **pairing request** signed "AI 助手 · MCP 会话". Click **Trust it** and it gets through — every command after that still follows whatever approval mode the device is in, exactly like any other controller.

## When the session drops

A hosted login lives only in the relay's memory. Restart the relay, or reset the connection, and the AI gets `LOGIN REQUIRED`.

A successful login also handed the AI a **rebind credential** starting with `wrb1.`, good for seven days. It keeps that itself and uses it to recover on the spot, without sending you back to the browser. Losing it costs nothing — the three steps above work again.

## What it can and cannot touch

- **The token never lands on disk.** A hosted session's credential lives in the relay's memory and is written nowhere.
- **File transfer is upload-only.** `wanctl_push` and `wanctl_pull` are off on the hosted endpoint — that "local path" would be a path on the server, not yours. Send files to a device with `wanctl_push_blob`.
- **Rotating the seed logs everyone out.** If the operator changes the relay's MCP seed, every session and every rebind credential dies immediately.
- **Say so when you are done.** Ask the AI to call `wanctl_logout`; that session's credential and its rebind credential expire together. Withdrawing a device's trust is done on that device's page.

> A hosted session has no trust store of its own, so the first time it reaches a device it may stop at a device-identity confirmation, reporting a fingerprint it cannot confirm for you. Either switch that AI to the local stdio setup, or ask the operator to turn on the opt-in on the relay (see "Enable the hosted MCP endpoint" in the self-hosting guide).
