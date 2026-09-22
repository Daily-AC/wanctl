# 0013 — Bind one conversation and reuse its authorized workspace connection

Date: 2026-09-22
Status: implemented on the feature branch; not released

## Problem

The first real development trial required a workspace reference on every
operation. Even after removing the extra short-command poll, opening a new
relay channel and TLS session for every operation remained visible latency.
An account-wide default would mix conversations. Reusing a channel without
rechecking authorization would extend the lifetime of an old permission.

## Decision

`wanctl mcp --workspace-session` is an explicit stdio-only mode. The host must
dedicate that process to one conversation. Enter binds the successful remote
workspace; data tools omit target/workspace from their advertised schemas and
receive the reference internally. Exit clears the binding only after a
successful remote close. A second enter requires exit first. Missing or failed
binding never falls back to a local tool or another device. Shared HTTP MCP
does not enable this mode and retains explicit references.

The mode exposes workspace operations and authentication/discovery tools.
Unbound transfer and device-management tools are not part of this profile.
It does not intercept the host's unrelated native tools. The host may restore
a new process with `attach`, which queries an existing workspace and binds it;
attach never creates a shell or restores a lost device process.

A conversation owns one serialized `WorkspaceLink`. It is keyed by relay,
transport, controller identity/credential, label, current device pin and
workspace reference. Changing any of them requires a new handshake. A
dedicated `workspace_hello` negotiates reuse, preventing older agents from
silently accepting an unchecked channel. Only workspace envelopes are allowed
on it. Unknown outcomes are not automatically replayed. A failed connection
is discarded while the workspace binding remains remote.

The device checks its current controller trust and asks the relay to
revalidate the original connection's credential and current device sharing
rights on each operation, and again after approval before acting. The relay
retains its own connection authorization record and uses the existing token
store and access rules. Controllers do not supply new permission claims or
send raw credentials to the device. Payloads remain under end-to-end TLS.
Already admitted execution has the existing device-owned lifecycle; this is
not a claim to undo effects or terminate an already approved command when a
credential is later revoked.

Cancel and exit use independent authenticated connections, so a shared
connection waiting on human approval cannot block them. Cancelling a pending
approval records its cancellation before the approval returns; it cannot run
or create a remembered rule after the workspace stopped. In-flight connection
creation is generation-bound, so an exit cannot be followed by an older dial
installing its socket into the closed scope. Cancellation callbacks finish
before the next request can reuse their socket.

## Compatibility and verification

Default stdio and shared HTTP retain their explicit-reference contract. The
new mode requires a compatible controller/MCP server, agent and relay
authorization endpoint. There is no silent downgrade on a failed negotiation.

Tests exercise both HTTP and WebSocket carriers with actual device shells and
files, credential/share/trust revocation, late approval, cancellation during
approval, reconnection, and a compiled stdio conversation-mode process.

The [second real Codex trial](../plans/2026-09-22-workspace-conversation-trial.md)
uses two independent MCP processes against an actual Linux host. It demonstrates
zero routing arguments on subsequent data operations, state isolation, network
recovery, process restart plus attach, and explicit exit.
