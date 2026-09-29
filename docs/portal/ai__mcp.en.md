# Connect an AI over MCP

MCP is how an AI calls wanctl directly — no skill to read, no command line to assemble. `wanctl_peers`, `wanctl_exec` and the rest simply appear in its tool list. These tools arrive two ways, and only one thing decides which: **can that AI start a `wanctl` process on its own machine?**

| | Use | Typically |
| --- | --- | --- |
| It can | Local stdio | Claude Code, Codex, Cursor |
| It cannot | Hosted endpoint (the host must support OAuth) | ChatGPT, claude.ai on the web, cloud agent runners |

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

## Hosted (HTTP, OAuth)

The endpoint is `https://relay.example.com/mcp`. Some relays answer on `/wanctl-mcp` instead, because the proxy in front of them has claimed the `/mcp` prefix; ask whoever runs it which one to use. It needs no file from your machine, and you never hand it a token.

OAuth is the only way in. The first time a host connects it gets a 401 whose header names where to discover the authorization server; it follows that and sends you to the portal. You sign in with GitHub as usual, the page names the client that is asking and the host your authorization will be delivered to, and you click **Allow**. No code to copy, nothing to paste back.

- **Claude Code**: `claude mcp add --transport http wanctl https://relay.example.com/mcp`, then type `/mcp` in Claude Code, pick wanctl and walk through the authorization.
- **A web AI such as ChatGPT or claude.ai**: in its custom connector, give it the same URL and set authentication to **OAuth** (not "no authentication"); it works out the rest of the discovery itself.

> The endpoint is public, but a request without an authorization gets 401: no handshake, and no devices.

The authorization belongs to the connector rather than to a session, so every new session it opens is still signed in, and other people and other connectors on the same endpoint authorize on their own without seeing anyone else's devices. To withdraw it, revoke the token labelled `oauth:` plus the client's name on the portal's access-token page, or ask the AI to call `wanctl_logout` — same effect. A connector in use renews its authorization by itself; one left unused for 30 days loses it, token included, and has to be authorized again.

The first time it then reaches a device, that device's **Waiting** page raises a **pairing request** signed "AI 助手 · MCP 会话". Click **Trust it** and it gets through — every command after that still follows whatever approval mode the device is in, exactly like any other controller.

> From v0.19.0 the hosted endpoint accepts OAuth only. The path that worked without it (`wanctl_login` with a one-time code, and a rebind credential starting with `wrb1.`) is gone: its logout was remembered only in relay memory, so a relay restart made a logged-out credential work again. A third-party hosted AI that does not support OAuth cannot use the hosted endpoint; an AI that can start a process on your machine uses the local stdio setup above.
>
> The hosted endpoint needs the operator to have given the relay a database, `WANCTL_PUBLIC_ORIGIN` and `WANCTL_PORTAL`, all three. Without any one of them `/mcp` answers 503 and names the one that is missing.

## What it can and cannot touch

- **Identity follows the authorization.** Every request carries an OAuth access token that names the account; after a revoke on the portal or `wanctl_logout`, the next request gets 401.
- **File transfer is upload-only.** `wanctl_push` and `wanctl_pull` are off on the hosted endpoint — that "local path" would be a path on the server, not yours. Send files to a device with `wanctl_push_blob`.
- **Rotating the seed means authorizing again.** Access tokens and stored authorizations are sealed with the relay's MCP seed; if the operator changes it, every authorization dies at once and each connector has to be authorized again.
- **Say so when you are done.** Ask the AI to call `wanctl_logout`, or revoke the token on the portal's access-token page; the authorization ends immediately. Withdrawing a device's trust is done on that device's page.

## v0.12.0: persistent remote workspaces

Call `wanctl_workspace` with `action=enter`, a target device and an absolute project root. Pass the returned `workspace` reference to subsequent read, write, edit, exec and poll calls. The device retains the shell directory, environment and request results. Each chat keeps its own reference; OAuth preserves identity, not a shared current directory.

When a local host dedicates one MCP process to one conversation, `wanctl mcp --workspace-session` can bind that process automatically. The CLI uses `wanctl workspace enter`, then `--workspace` or that terminal's `WANCTL_WORKSPACE`. CLI and stdio MCP can continue each other's workspace when using the same local controller identity; hosted OAuth has a separate identity.

After a disconnect, query the original request ID instead of repeating an uncertain operation with a new ID. Finish with `wanctl_workspace action=exit`. Workspaces do not survive an agent restart. Upgrade the controller, relay and device agent. See the [workspace guide](https://github.com/Daily-AC/wanctl/blob/main/docs/workspaces.md).

> First-contact trust follows the host's approval flow. When the operator enables this capability, `wanctl_trust_server` can record an independently checked fingerprint. Continue under sufficient existing authorization; otherwise obtain the required confirmation. A changed fingerprint must stop the connection for investigation, never silently overwrite the old pin. Without the operator opt-in, use the local stdio trust flow.
