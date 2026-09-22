# Conversation binding and connection reuse: real use, 2026-09-22

## Result

Within the dedicated wanctl MCP toolchain, the development loop now behaves
like staying in one workspace: enter once, then read/edit/run using project
paths and commands. This does not redirect Codex's native local tools, and it
does not infer conversation identity for a shared web MCP endpoint.

The current Codex conversation used a thin stdio JSON-RPC client to invoke two
real `wanctl mcp --workspace-session` subprocesses. That client did not inject
workspace or target arguments. The Go MCP implementation bound the calls.
Both processes shared one authenticated controller identity but had independent
bindings and connections to an isolated Linux relay/agent, running as uid
65534. The relay was loopback-only and reached through a private SSH tunnel.

## Actual development task

The existing CSV order-summary tool from the previous trial was the starting
point. Codex added `--flavor NAME`, zero totals when no flavor matches, and
validation of malformed paid rows even when the filter excludes their flavor.
All application reads, edits, execution and artifact collection used MCP.
SSH was used only for environment setup and cleanup.

The completed program passed 12 tests. Filtering the sample for 苹果 returned
quantity 3 and revenue `0.40`. The project source, tests and usage guide were
retrieved through MCP and checked against the returned SHA-256 values.

## Evidence

| Observation | Result |
|---|---|
| Total recorded MCP calls | 30 |
| Successful data/exec/poll operations | 22 |
| Those operations containing target or workspace arguments | **0** |
| Median of the 22 successful bound operations | **584.5 ms** |
| Short exec samples | 7 |
| Median / range of short exec | **591 ms / 75–2,131 ms** |
| First enter in the two processes | 4,828 ms / 4,804 ms |
| Accepted device connections in the device audit | **5** |

The five device connections correspond to two initial entries, one process
restart/attach, and two independent exit requests. Reads and commands reused
the admitted channels. Timing varies with the network: these are samples on
this route, not a controlled universal benchmark. The preceding trial's short
exec median was 4,837 ms after the earlier polling fix.

## Isolation and continuity

Process A set `WORKSPACE_LABEL=orders`; process B set it to `other`. Alternating
calls returned their respective values and directories without route arguments.
Entering another workspace while A was still bound was refused locally.

During a delayed run of the actual 12-test suite, the SSH tunnel was terminated
while both MCP processes remained alive. Restoring the tunnel let A poll the
original job without re-entering or supplying a workspace reference. Its
output and exit code were intact. The command's marker file contained one
`done`, and the environment still contained `orders`.

A's MCP process was then restarted. A data call before attach was refused;
`attach` with its saved reference restored the existing remote state. The
following command, again without route arguments, printed `orders` and the
correct filtered report. Exit without a reference closed A; further execution
was refused. B still printed `other` and exited independently.

## Authorization and limits

Separate real relay/agent integration tests cover revoking the original token,
the share, and device trust on an already reused connection, plus revocation
and cancellation while a policy approval is pending. These checks use controlled
in-memory token/share stores with actual sockets and policy handlers; they are
not claimed as live production revocation tests.

This mode requires one stdio process per conversation and a compatible relay
and agent. Existing shared HTTP clients continue using explicit references.
Interactive PTYs, service previews and interception of native host tools remain
outside this delivery.

## Retained evidence and cleanup

Local ignored evidence is under `artifacts/conversation-20260922/`:
`calls.jsonl`, `assessment.json`, retrieved project files in `orders/`, Go
validation logs and `cleanup-result.json`. The device audit confirmed five
accepted connections. Test processes and the remote directory were removed;
temporary local and remote credentials were deleted. Existing public services
were not upgraded.
