# 0017 — Desktop act relays OpenCU's MCP interface

Date: 2026-10-17
Status: implemented; real Windows acceptance pending

## Decision

Extend `wanctl act` / `wanctl_act`, without another MCP tool, with two OpenCU
operations: `discover` and `{tool, arguments}`. Both select `target` exactly as legacy act does and
exclude legacy `actions`, `screenshot_id` and `region`. Discovery returns the
peer's `serverInfo`, `instructions` and complete, paginated tool list. Prefer
those tools when installed; otherwise ask the person to install OpenCU, while
legacy `actions[]` remains available with its existing validation, screenshot
references, approval policy, helper deadline and input safeguards.

The public integration is OpenCU's newline-delimited JSON-RPC MCP server.
Its engine protocol is internal. The MCP layer owns observations, argument
normalization, batching and error behavior; duplicating it would couple wanctl
to OpenCU internals. No tool names or application allowlist live in wanctl.
OpenCU stays separately installed and separately released.

The authoritative upstream contracts reviewed for this decision were
`opencu/docs/host-integration.md` and
`opencu/docs/adr/0001-mcp-as-host-interface.md` (2026-10-17).

## Session identity and lifetime

The key is **authenticated controller fingerprint + this device**. Each device
agent owns its desktop service, keyed by the authenticated fingerprint; neither
the caller's label, CLI process nor MCP transport session is part of that key.
OpenCU mode takes `target`, with no project workspace, shell, root directory or
workspace ownership check. This product decision replaces the initial workspace
binding: operating a desktop must not require entering a project.

All conversations under one controller identity share that device's OpenCU
connection and stop latch. Another conversation's observation can invalidate
element indexes or screenshots; the agent must observe again before input.
Serialization prevents concurrent calls, but does not make an observation and a
later action atomic across conversations. A person's stop blocks **every**
conversation of that controller on that device until fresh explicit approval.
Different controller fingerprints have separate connections/latches, still
subject to the same device-wide busy lock.

CLI and ordinary stdio/hosted MCP use the existing target routing and fresh
authenticated relay legs. Transient CLI/MCP/relay disconnects do not clear a stop
or close an idle peer. An in-flight disconnect cancels that call and discards
its connection, returning unknown outcome without replay. Unrelated workspaces
retain their existing behavior and have no OpenCU lifecycle hooks. The restricted
stdio `--workspace-session` profile remains shell/file scoped; use ordinary
`wanctl mcp` for desktop tools. Both profiles' tool counts remain unchanged.

Connections close after ten minutes idle, on agent shutdown, and before
automatic update installation. Idle peers do not count as desktop Busy. The
update hook holds the existing desktop lock while draining peers and installing;
failure releases the lock without clearing stop latches. The stop latch is
retained independently of the peer, including after idle close or failure, until
a fresh human approval. Agent restart loses this in-memory state; agents must
never restart it or change identity to circumvent a person's stop. After idle,
failure or restart, observe again before input. Existing shell/console/update
Busy rules remain unchanged.

## User-session launch

A persistent form of `__desktop` reuses both existing launchers. A logon agent
spawns its helper with stdio pipes. A session-0 LocalSystem agent selects the
active console user using WTS and starts the same helper using the limited
primary token, `winsta0\default`, a user environment and the existing DACL-
protected single-instance named pipes with spawned-PID verification.

Only that helper reads `%LOCALAPPDATA%\OpenCU\opencu.json`. Missing/unreadable
manifests return `OPENCU_NOT_INSTALLED` and the exact per-user PowerShell command:

```powershell
irm https://github.com/Daily-AC/opencu/releases/latest/download/install.ps1 | iex
```

The helper validates absolute manifest paths and launches exactly `mcp.command`
and `mcp.args`, with no shell and no console window. It refuses session 0 and
an elevated inherited token. A manually elevated logon agent therefore needs
to be restarted normally; it cannot use OpenCU to elevate input. No manifest
is read from SYSTEM's profile. Non-Windows remains `desktop: Windows only`.

## Fidelity, limits and failure

Initialize, send `notifications/initialized`, discover `tools/list` (including
pagination), and forward `tools/call` once. Result content, including PNG base64,
`isError`, `_meta`, content extension fields and large integers are retained.
OpenCU arguments are captured as raw JSON before the MCP SDK can decode
numbers through float64; other tools keep their existing decoding.
OpenCU content bypasses the MCP wrapper's normal device-text escaping. A
`PERSON_ACTIVE` result keeps its original content and gains the existing host
instruction to ask the person and never retry automatically. JSON-RPC errors
are returned as their original error object in `rpc_error`, without replay.
Stderr is drained and discarded, never mixed into MCP, approvals or audit.

A peer line/result is bounded at 64 MiB, including base64 expansion (not 64 MiB
of PNG). The helper/device result header carries `opencu_bytes`; the complete
JSON envelope follows in FrameData chunks of at most 256 KiB. This stays below
the existing 16 MiB frame limit and WebSocket's `MaxFrame + 64 KiB` limit.
HTTP's existing batching, upload bounds and finite down-poll windows carry the
chunks as a stream, without increasing relay limits. Over-limit, truncated or
broken exchanges are unknown outcomes; there is no truncation or retry.

CLI discovery prints JSON. Tool calls decode each image content item into a
separate PNG file without recompressing it. The default is a private temporary
directory; `-o DIR` selects an output directory. Files have unique names and 0600
permissions, including in an existing directory. JSON stdout retains text,
metadata and content order, replacing each image's base64 `data` with its absolute
`path` and decoded `bytes`. A `result_file` path points to the complete raw MCP
JSON saved alongside the PNGs. Thus a shell-driven agent can open images directly
with its file/image reader. `-o -` prints the complete original MCP result, writes
no image files and keeps any PERSON_ACTIVE note on stderr. `--arguments-file`
avoids putting private typed/pasted text in argv. MCP content is unchanged.

## Authorization, stop latch and serialization

The existing Exec capability and act approval gate apply, including discovery.
Approval, audit and notification summaries contain only `opencu <tool> <app>`;
`app` is omitted when absent and discovery uses `opencu discover`. Control
characters and excessive label lengths are removed/bounded. No other arguments,
text, paste payload, screenshot or peer diagnostics enter the audit log.

OpenCU and legacy desktop calls share the existing device TryLock. OpenCU
holds it across approval and execution, so another call cannot slip between
checking a stop and applying the human decision. There is one in-flight call
per connection. Busy requests are refused, never queued or replayed.

`USER_STOPPED` or `STOPPED_BY_PERSON`, either in `_meta["opencu/error"]` or the
prefix of the first text content, latches this controller stopped on this device, across all its conversations.
The latch is outside the child process and survives idle close and update
preparation. Discovery also cannot reconnect around it. The next request asks
the existing human approver **directly**: bypass, remembered rules and late
grants cannot clear the latch. Denial/no approver leaves it stopped and creates
no connection. Only a fresh explicit approval closes the old peer and permits
a new one. That decision is not saved as a rule. Another conversation cannot
reconnect around the latch; agents must not change identity or restart anything
to circumvent the person's stop.

Process/stream failure during a call discards the connection and reports that
the outcome is unknown: observe again, never replay. Neither the MCP layer,
helper, device service nor desktop client retries the tool call.

## Evidence and remaining acceptance

Tests compile a standalone Go MCP peer and exercise real OS pipes, initialization,
pagination, dynamic schemas, missing manifests, multi-MB PNGs, exact metadata,
process reuse across conversations, controller isolation, idle close, shutdown/update cleanup,
stop/approval races, bypass/saved-rule refusal, crash/no replay and device-wide
serialization with legacy act. Additional tests carry a result larger than a
16 MiB frame through both real relay carriers and through CLI/MCP renderers.
CLI tests verify multiple decoded PNG files, their byte fidelity and private
permissions in both default temporary and explicit output directories.
Existing legacy tests remain unchanged. Windows launch code is cross-compiled;
token/session/physical-Esc behavior still needs the isolated reviewer runbook:
[Windows acceptance](../plans/2026-10-17-act-opencu-acceptance.md).
