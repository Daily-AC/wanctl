# 0017 — Desktop act relays OpenCU's MCP interface

Date: 2026-10-17
Status: implemented; real Windows acceptance pending

## Decision

Extend `wanctl act` / `wanctl_act`, without another MCP tool, with two OpenCU
operations: `discover` and `{tool, arguments}`. Both require a workspace and
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

The key is **authenticated controller fingerprint + explicit workspace ID**,
inside one device agent. An MCP transport connection, OAuth account, relay
socket or CLI process does not identify a conversation: hosted clients can
create a new transport session for every call, and one identity can own several
conversations (ADRs 0012–0014).

Enter a workspace once and retain its reference. CLI calls accept `--workspace`
or, in OpenCU mode only, `WANCTL_WORKSPACE`. Each can open a fresh authenticated
relay connection while the device retains the same user helper and OpenCU MCP
connection. Default/hosted MCP passes `workspace` explicitly. The existing restricted
stdio `--workspace-session` profile remains shell/file scoped; use ordinary
`wanctl mcp` for desktop tools. This keeps both profiles' tool counts unchanged.
OpenCU calls use independent authenticated legs, not the reusable shell link. The device
checks workspace ownership and its live context on each operation and again
after approval. References grant no authority.

The workspace is the logical controller session. Explicit workspace exit ends
it; cancellation invalidates it. Ending a transient CLI process or MCP transport
does not end a workspace (and does not erase a person's stop). This also lets a
replacement MCP process attach to the same conversation. Hosts must exit their
workspace when the conversation finishes. A disconnected in-flight request
cancels that call and discards its connection, with an unknown-outcome result.

Each connection closes after ten minutes without a call, on workspace end,
on agent shutdown, and before automatic update installation. Idle connections
do not count as desktop Busy. The update hook holds the existing desktop lock
while draining peers and installing; failure releases the lock without clearing
stop latches. Existing shell/console/update Busy rules remain unchanged; a
separately open reusable shell link can still count as work. Connection state
is ephemeral: after idle, failure or restart, observe again before input.
Agent restart also loses the workspace, so an old reference fails closed.

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

CLI discovery prints JSON. Tool calls save the complete raw MCP result in a
private JSON file (`-o FILE`, otherwise a unique `opencu-result-*.json`), and
print JSON with text, metadata and decoded image byte sizes in place of base64.
`-o -` outputs the complete raw result. `--arguments-file` avoids putting
private typed/pasted text in argv. MCP returns native content items unchanged;
wanctl does not resize or recompress them.

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
prefix of the first text content, latches the workspace's OpenCU session stopped.
The latch is outside the child process and survives idle close and update
preparation. Discovery also cannot reconnect around it. The next request asks
the existing human approver **directly**: bypass, remembered rules and late
grants cannot clear the latch. Denial/no approver leaves it stopped and creates
no connection. Only a fresh explicit approval closes the old peer and permits
a new one. That decision is not saved as a rule. Agents must not create another
workspace or restart anything to circumvent the person's stop.

Process/stream failure during a call discards the connection and reports that
the outcome is unknown: observe again, never replay. Neither the MCP layer,
helper, device service nor desktop client retries the tool call.

## Evidence and remaining acceptance

Tests compile a standalone Go MCP peer and exercise real OS pipes, initialization,
pagination, dynamic schemas, missing manifests, multi-MB PNGs, exact metadata,
process reuse, idle close, workspace ownership/end, shutdown/update cleanup,
stop/approval races, bypass/saved-rule refusal, crash/no replay and device-wide
serialization with legacy act. Additional tests carry a result larger than a
16 MiB frame through both real relay carriers and through CLI/MCP renderers.
Existing legacy tests remain unchanged. Windows launch code is cross-compiled;
token/session/physical-Esc behavior still needs the isolated reviewer runbook:
[Windows acceptance](../plans/2026-10-17-act-opencu-acceptance.md).
