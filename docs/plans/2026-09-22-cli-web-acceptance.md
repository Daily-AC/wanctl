# CLI and ChatGPT web acceptance, 2026-09-22

## Delivery

The feature branch now exposes persistent workspaces through both CLI and MCP.
It has not been merged or released. A temporary hosted candidate is available
through the existing `/wanctl-mcp` alias; the normal `/mcp` endpoint remains
on the production binary.

The local Codex development trial passed. ChatGPT web authenticated with OAuth,
discovered the workspace tools and completed three read-only calls, but its
first device trust call was blocked by the platform's safety check. The web
development task is **not accepted yet**. A working login/tool list is not a
substitute for file edits and execution through the web UI.

## What changed

- CLI lifecycle: `workspace enter`, `attach`, `status`, `poll`, `cancel`, `exit`.
- Exec/read/edit/write accept `--workspace` or caller-scoped `WANCTL_WORKSPACE`.
- Exec waits and drains output, or submits with `--async`; request IDs support
  reconnect and deduplication. Interrupting a waiter leaves the device task alive.
- Dedicated stdio MCP can inherit the same reference on process startup.
- OAuth `wanctl_id` initializes an authenticated identity instead of falsely
  reporting an uninitialized identity as logged out.
- CLI help, the generated contract and the BFS architecture series were updated.

No account-wide current workspace, additional authorization system, Python
kernel, interactive PTY or automatic interception of native host tools was added.

## Real local Codex task

The current Codex harness used the compiled CLI to create and modify a Python
CSV summary program on a real Linux device. All application reads, writes,
edits and test execution went through wanctl. SSH only prepared the isolated
environment, carried the private relay connection, and interrupted/restored
that connection for the recovery probe.

The program uses Decimal arithmetic, supports an exact name filter, validates
rows before filtering and emits JSON. Six unittest tests passed. The Chinese
sample filtered to 苹果 produced quantity 3 and amount `0.40`.

| Probe | Observed result |
|---|---|
| Separate CLI processes | Environment and shell cwd persisted |
| File tools after shell `cd src` | Relative paths still resolved at project root |
| CLI then MCP | MCP inherited the reference and retained cwd/environment |
| MCP then CLI | File changes and the updated exported variable remained visible |
| Local connection deliberately stopped | Poll failed with connection refused |
| Connection restored | Original request completed with exit 0 |
| Same request submitted again | Marker file contained `once`, not two copies |
| Explicit exit | Further execution was refused, with no fallback |

After entry, the saved trace has 17 CLI calls: 15 successes and the two expected
failures (disconnected poll and post-exit exec). It also has four successful
MCP calls, all without target/workspace arguments. The calling environment
supplied the binding. The MCP client was a thin JSON-RPC driver launched by
Codex, not a modification to Codex's native tool backend.

The median of the successful CLI calls was 94 ms on this particular LAN/SSH
route. The four MCP samples were 13–155 ms with connection reuse. These are
small, differently composed samples, not a controlled CLI-versus-MCP benchmark
or a WAN performance guarantee.

## Actual ChatGPT web path

A separate development connector named **wanctl Workspace Preview** was
created through the ChatGPT UI and connected through the existing wanctl OAuth
consent flow. The connector's normal permission setting was retained.

The old `/mcp` endpoint advertised 21 tools. The candidate `/wanctl-mcp`
advertised 22, including `wanctl_workspace` and workspace arguments on data
tools. This was checked at the origin proxy and then in ChatGPT's actual
connector schema display. The two endpoints intentionally use different relay
registries: only the isolated test agent connects to the candidate.

ChatGPT called enter and encountered the expected first-contact identity check.
The fingerprint matched the one independently read from the test agent's
certificate through SSH. Its ensuing `wanctl_trust_server` invocation was
blocked, with the reported error:

> 此工具调用被 OpenAI 的安全检查屏蔽。请仔细检查你发送的内容。

The error did not identify a narrower rule. It cannot be attributed confidently
to a particular plugin permission or approval subsystem. No alternate route was
used to install the pin, and no plugin permissions were loosened. There is no
successful web workspace, write, exec, isolation or close result to claim.

An independent read-only continuation called `wanctl_id`, `wanctl_status` and
`wanctl_peers`. All three returned successfully. The downloaded raw tool trace
confirms OAuth login and one visible test device whose identity remained
`unpinned`. The browser response and the trace are retained locally.

This also remains insufficient to prove the exact MCP protocol-session lifetime
of ChatGPT. The automated transport test deliberately recreates that session on
every call and still succeeds; that is a resilience test, not a measurement of
ChatGPT's protocol headers.

## Automated verification

- Standard and lark test suites: all 32 tested packages passed in each run.
- Standard/lark vet and lark build passed.
- Windows amd64 and Android arm64 cross-builds passed (not device execution).
- Workspace/authorization/identity integration tests passed under the race detector.
- The new compiled CLI test covers output paging, exit status, matching scripts,
  independent workspaces, cancellation, replay conflicts, file operations and
  two freshly started stdio servers inheriting a CLI-created workspace.
- Catalog/help and the CLI integration test were repeated after the final help
  and instruction wording changes.

## Review and remaining acceptance

The macro architecture explanation is
[chapter 7](../learning/remote-workspace/07-cli-mcp-oauth.md); the usage contract
is [workspaces.md](../workspaces.md). The lifetime decision is
[ADR 0014](../adr/0014-cli-workspace-binding.md).

The remaining external condition is the ChatGPT first-trust safety barrier.
Once it is resolved through an authorized path, continue the saved web task,
then verify another chat's isolation, reconnect/continuation and explicit exit.
Local tests do not replace those pending user-visible checks.

Local ignored evidence is in `artifacts/cli-web-20260922/`: `assessment.json`,
CLI/MCP call traces, the downloaded web tool trace, application source, test
logs and an `ACCEPTANCE.md` with private review links and exact replay commands.
No credentials or private chat/device identifiers are committed with this report.

The local trial workspace was explicitly closed. The isolated preview service
and test agent remain available for the owner's next-day review. Production
relay and PostgreSQL start times were unchanged. Automatic migrations were
disabled for the preview. Cleanup instructions and a narrowly scoped cleanup
script accompany the private acceptance notes.
