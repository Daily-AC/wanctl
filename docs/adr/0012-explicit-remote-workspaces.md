# 0012 — Explicit remote workspaces survive controller connections

Date: 2026-09-21
Status: implemented on the feature branch; not released

## Problem

The legacy persistent shell is keyed by controller fingerprint. Hosted MCP
derives that fingerprint per namespace, so unrelated conversations can share a
shell. A connection lost during execution cancels and resets that shell under
ADR 0011. Native file tools require absolute device paths and have no project
association. These behaviors do not provide an independent remote workspace.

## Decision

Introduce one workspace envelope in the existing encrypted device protocol.
An old agent rejects its kind rather than ignoring an optional workspace field
and running an operation somewhere else. Inside it, native file operations
continue through the existing capability and device policy gates.

Each workspace belongs to its authenticated controller fingerprint and has a
random ID, absolute project root, independent command shell, and command
ledger. File paths resolve relative to the root. An explicit exec cwd resolves
against the root; omitting cwd preserves the shell's current directory. The
directory used for exec policy evaluation is the project root unless an
explicit cwd is supplied. This is a policy scope, not an OS sandbox.

The controller chooses the workspace ID before opening, so an uncertain open
can be recovered by the same reference. Execution similarly reserves a caller
request ID before approval or starting the command. Duplicate ID/content
returns the original result, including pending approval. ID/content conflicts
are refused. The in-memory ledger provides deduplication only for that live
workspace; it does not promise exactly-once effects across device restarts.

The device owns execution contexts and bounded output. No controller socket
or request context owns a workspace command. Each shell accepts one command
at a time; busy is explicit. Outputs are read in bounded pages with byte
cursors, retaining at most 8 MiB per workspace. The ledger keeps at most 128
commands and 1 MiB of command text. Reaching a limit is explicit; entries are
never silently removed and then allowed to execute again. Each command uses
the existing 30-minute job deadline.

Exit closes the workspace. Cancel targets the active request and invalidates
its shell under ADR 0011; the workspace's result ledger remains queryable.
Network loss does neither. Shell failure never creates a replacement shell
silently. Device process restart loses workspace state. Open workspaces keep
the auto-updater's existing Busy guard active.

MCP receives one lifecycle tool, `wanctl_workspace`. Existing exec, async,
poll, read, edit and write tools accept a workspace reference instead of
target. References include the canonical device route and workspace ID; they
are locators, not credentials. They are passed explicitly across calls.
Neither an OAuth token nor an MCP transport session is assumed to identify an
AI conversation. A local harness can inject the reference per conversation;
automatic routing of that harness's own tools requires its backend adapter.

Existing short-lived delegation permits only synchronous one-shot execution.
Workspace envelopes are refused on that path. No second permission system or
implicit permission grant is introduced by entering a workspace.

## Boundaries

- This is a persistent command shell, not a PTY or interactive Python kernel.
- Files and commands run on the device. MCP does not redirect unrelated host
  tools or provide service preview forwarding.
- Filesystem mutations still use the existing result-unknown/hash recovery
  contract; command request IDs do not make file edits transactional.
- Connection recovery re-authenticates. It does not revive revoked access.
- Stopped/closed workspaces never fall back to another device or local tools.

## Evidence

`TestWorkspaceEndToEnd` uses actual WebSocket and HTTP relay transports, TLS,
OS shells and files. It checks isolation, disconnect recovery, deduplication,
conflicting IDs, ownership and explicit close. MCP integration uses real HTTP
requests with fresh OAuth-authenticated MCP sessions on every call, plus a
compiled `wanctl mcp` subprocess over stdio. Device tests exercise policy,
delegation refusal, cancellation, approval/close races and resource bounds.

The learner-oriented explanation is in
[the Chinese BFS series](../learning/remote-workspace/README.md).

## Immersion follow-up

A real Codex-driven Linux task found that encoded scripts launched child
shells and lost cwd/environment, and that the controller-side short wait
added a second connection. Workspace scripts now use a dedicated action and
device-verified source matching the original encoded policy command, executed
inside the existing matching shell. The device can wait up to 250 ms for a
normal MCP exec, bounded at one second at protocol level, while command
ownership remains independent of the connection. See the
[trial evidence](../plans/2026-09-21-workspace-immersion.md).
