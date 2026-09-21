# Remote workspaces over MCP

This feature is implemented on `feat/remote-workspace-session`. It is not in
the existing public release or hosted MCP deployment. Both the MCP server and
device agent need this implementation. The relay byte transport is unchanged.

A workspace owns a project root and independent persistent command shell.
The returned reference is explicit so several conversations can use the same
account without sharing a mutable default directory or shell.

## Enter and work

Call `wanctl_workspace`:

```json
{"action":"enter","target":"lab","root":"/srv/app"}
```

Save the returned `workspace` value verbatim. Read the project's instructions
before editing. The following objects are arguments to the named MCP tools;
`<workspace>` is replaced with that returned reference.

```json
{"workspace":"<workspace>","path":"AGENTS.md"}
```

Use `wanctl_read`, `wanctl_edit` and `wanctl_write` as before, with workspace
instead of target. Relative file paths start at the project root, regardless
of a terminal's current directory. Existing atomic edits and hash checks apply.

Execute with `wanctl_exec`:

```json
{"workspace":"<workspace>","request_id":"set-env","command":"export MODE=test"}
```

```json
{"workspace":"<workspace>","request_id":"check-env","command":"printf '%s\n' \"$MODE\""}
```

Each new command needs a new request ID. If omitted, one is generated and
returned. The result is available as JSON text and MCP structured content.
For a matching shell language, `script` with `interp` executes inside the
workspace's shell, so script `cd` and exported variables persist too. The
device verifies the script against the encoded command used for policy. An
older workspace agent that lacks this behavior rejects the dedicated script
action; update that agent instead of assuming state was kept. Other
interpreters can still be invoked explicitly as ordinary commands.

Short commands wait up to 250 ms on the device before returning. This avoids
opening another connection to poll a command that already finished.
`done=false` means the request is still approving/running. `code` is final
only when `done=true`. Poll with `wanctl_exec_poll`:

```json
{"workspace":"<workspace>","job_id":"check-env","offset":0}
```

Use the returned `next_offset` on the next poll. Even after the command is done,
continue while `next_offset < retained_bytes`. `output_bytes` counts produced
bytes; `truncated=true` means some bytes are not retained. Output is bounded
at 8 MiB across the workspace. The response does not claim a full log exists
elsewhere. For expected large output, redirect it to a project file and inspect
that file explicitly under the usual policy.

The shell runs one command at a time. `wanctl_exec_async` with workspace uses
the same persistent shell and returns without the short wait; it does not
create an independent parallel terminal. On a busy response, poll the active
request first. No stdin/PTY, oneshot or elevation is supported in this mode.

## Reconnect, cancel and exit

```json
{"action":"status","workspace":"<workspace>"}
```

The workspace survives MCP reconnects and a caller ceasing to wait. If the
result of a command is unknown, poll its existing request ID or resubmit the
same ID and identical command/cwd. Never use a new ID to retry an uncertain
operation. A device restart loses the shell and ledger; an unavailable
workspace must not silently be recreated or redirected elsewhere.

```json
{"action":"cancel","workspace":"<workspace>","request_id":"long-build"}
```

Cancellation invalidates the shell, while its result remains queryable. Close
that workspace explicitly and enter a new one when ready. To end work:

```json
{"action":"exit","workspace":"<workspace>"}
```

The existing device permissions still apply on every operation. A workspace
reference is not an access token. Short-lived delegated access is rejected for
persistent workspaces. Limits are listed in `wanctl help wanctl_workspace`.

## Host integration

A harness can store the reference on its conversation object and inject it
when invoking the wanctl tools. To route the harness's original filesystem and
execution tools too, implement a workspace backend in that harness. Do not
store a current workspace by account, OAuth token, or shared MCP connection.

For a local development build:

```sh
go build -o bin/wanctl-workspace .
```

Use that binary's `mcp` subcommand as the stdio MCP command, and its `agent`
subcommand on an isolated test device/configuration with the usual relay,
login and independently verified pairing. This build command does not install,
update or restart the existing public deployment.

## Verification

```sh
go test ./internal/client -run TestWorkspace -v
go test ./internal/mcp -run TestWorkspaceThroughHTTPMCPAcrossFreshSessions -v
go test -race ./internal/agent ./internal/client ./internal/mcp -run 'TestWorkspace|TestDelegatedGrantCannotOpenPersistentWorkspace'
```

These use real loopback servers, authentication, processes and files. They do
not establish performance gains in an actual web AI or replace Windows and
Android device testing. See the [architecture decision](adr/0012-explicit-remote-workspaces.md)
and [Chinese architecture series](learning/remote-workspace/README.md).

An [actual Codex-driven Linux development trial](plans/2026-09-21-workspace-immersion.md)
records the task, problems found, fixes, timing samples and remaining friction.
