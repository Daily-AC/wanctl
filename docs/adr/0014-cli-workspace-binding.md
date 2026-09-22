# 0014 — Carry a workspace across CLI and local MCP processes

Date: 2026-09-22
Status: implemented on the feature branch; not released

## Problem

The device already owns workspace shell state, but the CLI cannot address it.
Process-local conversation binding alone also cannot survive a host replacing
its MCP child. OAuth solves caller authentication; sharing one mutable current
workspace by OAuth identity would mix independent chats.

## Decision

Add `wanctl workspace enter|attach|status|poll|cancel|exit`. Exec, read, edit
and write accept `--workspace REF`, defaulting to `WANCTL_WORKSPACE` in the
calling environment. No default reference is written into account settings.
A reference conflicts with `--target`; invalid or closed references never
fall back to legacy device selection. Enter returns the exact reference and
prints the chosen ID before submitting, allowing uncertain opens to recover.

The dedicated stdio conversation mode also accepts an initial reference from
`WANCTL_WORKSPACE`. The harness can pass the same reference to a replacement
process. Default stdio and shared HTTP retain explicit request arguments and
do not consume this variable as a mutable shared binding.

The CLI uses the existing workspace client and device protocol. Synchronous
exec drains output pages and propagates the exit code. Async submission returns
JSON; poll consumes one page by request ID and byte offset. Interrupting the
CLI waiter leaves the remote task running. Cancel and exit are explicit.
Matching-language scripts use the persistent shell through the same source
validation as MCP. Unsupported oneshot/elevation options are rejected.

An environment variable is sufficient for this lifetime contract. No local
daemon, ambient account context, or second authorization system is introduced.
The caller sets and clears its own binding: a child process cannot rewrite
its parent's environment. Connection reuse across independent CLI invocations
is not part of this change.

## Identity boundary

Workspace ownership remains the authenticated controller fingerprint. Local
CLI and stdio using the same config can hand off a workspace. Hosted OAuth MCP
has its own derived controller identity and cannot take over a locally created
shell just because the namespace matches. The two can still work on the same
device files under the existing policy. Cross-controller shell takeover would
need a separate explicit design.

## Evidence

Compiled CLI processes exercise actual relay, TLS, shell and filesystem state,
including scripts, output paging, exit codes, deduplication, cancellation,
isolation and close. Two separately started MCP processes inherit the same
CLI-created reference and continue its shell. A real Linux trial uses CLI and
MCP in both directions, including network interruption. See the
[acceptance record](../plans/2026-09-22-cli-web-acceptance.md) for the separate
ChatGPT web result and its unresolved first-trust approval barrier.
