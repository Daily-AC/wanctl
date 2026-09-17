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

## Web AI that keeps no session (ChatGPT, claude.ai)

A web AI like ChatGPT or claude.ai opens a brand-new MCP session for every single tool call. The per-session login below cannot survive that: the call right after a successful login reports LOGIN REQUIRED. There is a second path for them.

In its custom connector, give it the same endpoint URL and set authentication to **OAuth** (not "no authentication"); it works out the rest of the discovery itself. On save it sends you to the portal: you sign in with GitHub as usual, the page names the client that is asking and the host your authorization will be delivered to, and you click **Allow**. No code to copy, nothing to paste back.

The authorization belongs to the connector rather than to a session, so every new session it opens is still signed in. To withdraw it, revoke the token labelled `oauth:` plus the client's name on the portal's access-token page, or ask the AI to call `wanctl_logout` — same effect.

> This path needs the operator to have given the relay a database, `WANCTL_PUBLIC_ORIGIN` and `WANCTL_PORTAL`, all three. Without any one of them the endpoint keeps only the session login below, and an AI's connector finds no authorization server to discover.

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

> The first time a hosted session reaches a device it gets a device-identity confirmation. It answers that one itself, calling `wanctl_trust_server` to record the fingerprint it just saw, and retries — you are not asked. What does reach you is the alarm afterwards: if that device's fingerprint ever changes, the connection fails closed and the AI reports both the recorded and the presented fingerprint and stops, instead of recording the new one. If the device really was reinstalled, unbind it once in the portal: that drops the recorded fingerprint, and the next call starts over at first contact. (The operator turns this on with the opt-in on the relay — see "Enable the hosted MCP endpoint" in the self-hosting guide. Without it the session stays blocked on first contact and that AI has to move to the local stdio setup.)
