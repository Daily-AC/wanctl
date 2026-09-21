# Remote workspaces over CLI and MCP

This feature is implemented on `feat/remote-workspace-session`. It is not in
the public release or the normal hosted `/mcp` endpoint. A separate temporary
acceptance deployment is described in the [CLI/web trial](plans/2026-09-22-cli-web-acceptance.md).
Both the MCP server and
device agent need this implementation. The relay byte transport is unchanged;
the conversation mode below additionally requires the new relay authorization
endpoint and matching agent support.

A workspace owns a project root and independent persistent command shell.
Default stdio and shared HTTP use explicit references, so several conversations
can use one account without sharing a mutable default directory or shell.

## CLI

```sh
wanctl workspace enter --target lab --root /srv/app
# Copy the workspace value from the JSON result, without changing it:
export WANCTL_WORKSPACE='<returned reference>'
wanctl read AGENTS.md
wanctl exec --request-id setup 'export MODE=test; cd src'
wanctl exec 'printf "%s\n" "$MODE"; pwd'
wanctl write notes.txt --content 'a project-root-relative file'
wanctl edit notes.txt --old 'a project' --new 'the project'
wanctl exec --async --request-id tests 'python3 -m unittest'
wanctl workspace poll --request-id tests --offset 0
wanctl workspace status
wanctl workspace exit
unset WANCTL_WORKSPACE
```

Each invocation is a new CLI process; the device keeps the shell alive.
`--workspace REF` is the explicit alternative to the environment variable.
The binding is inherited by this terminal/harness's children only. Nothing
is saved as an account-wide current workspace. Supplying both a bound workspace
and `--target` is rejected. A stale binding never falls back to legacy exec.

Normal workspace `exec` waits, drains output pages, and propagates the remote
exit code. It prints the workspace and request ID to stderr before submitting.
Ctrl-C stops waiting; use `workspace poll` to resume or `workspace cancel
--request-id ID` to stop the command and invalidate its shell. `--async`
returns one JSON response immediately. A CLI poll prints one JSON page; keep
using `next_offset` until `done=true` and all retained bytes are consumed.
Workspace output is the shell's merged textual output, not a binary pipe.

`workspace attach --workspace REF` inspects an existing workspace; a child
process cannot change its parent's environment, so set `WANCTL_WORKSPACE`
explicitly when binding a new terminal. To recover an uncertain open, use
`workspace enter --workspace REF --root /the/same/root`. References require the
same controller identity. CLI and local stdio sharing a config directory can
resume each other's workspaces. Hosted OAuth MCP derives a separate controller
identity and creates its own workspaces, even under the same user account.

## One dedicated process per conversation

```sh
wanctl mcp --workspace-session
```

Use this only when the host dedicates that MCP process to ONE conversation.
Do not put multiple conversations through the same process. It cannot be used
with `--http` and does not redirect the host's unrelated native tools.

If this process inherits `WANCTL_WORKSPACE`, it starts bound to that reference.
This also supports hosts that restart the MCP subprocess: the host retains the
reference in its own conversation environment. The first device operation still
checks access and workspace availability; startup never creates a replacement.
Default stdio and HTTP do not read this environment variable as a binding.

Enter once with target and root. Then use the ordinary tools without target
or workspace arguments, for example `wanctl_read({"path":"README.md"})` and
`wanctl_exec({"command":"python3 -m unittest"})`. The server injects the binding
and reuses the authenticated device connection. Workspace enter is the only
place to choose a different working location, and requires exiting first.

`wanctl_workspace({"action":"exit"})` closes the bound workspace. Subsequent
data calls are refused until another enter/attach. A network error retains the
binding. If the MCP process itself restarts, restore it with
`wanctl_workspace({"action":"attach","workspace":"<saved reference>"})`;
attach never creates a replacement shell.

Reusing transport does not cache authorization. The device rechecks controller
trust, original relay credential and current sharing rights for each operation.
Cancel and exit use separate connections to avoid waiting behind approval.

## Enter and work

The following explicit-reference form remains the default for shared HTTP and
stdio processes that can serve more than one conversation.

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
The [conversation-mode follow-up](plans/2026-09-22-workspace-conversation-trial.md)
records actual work without routing arguments and with reused connections.
