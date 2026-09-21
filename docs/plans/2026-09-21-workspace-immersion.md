# Remote workspace immersion trial, 2026-09-21

## Verdict

The workspace supports a continuous real development task. It does not yet
make remote work transparent in the current Codex session. Two concrete
problems found during use were fixed: multiline scripts lost shell state,
and short commands paid for a second network connection just to fetch their
already-completed result. Explicit workspace routing and connection setup
latency remain visible.

## What actually ran

The current Codex conversation drove the development MCP server directly
through a temporary stdio JSON-RPC client in the available persistent Node
tool. No second model was delegated the task. The conversation's installed
wanctl connector lacked the new workspace schema; a generic gateway had no
registered providers. The temporary client provided transport, not automatic
workspace injection or replacement of Codex's native file/shell tools.

A real Linux x86_64 host ran isolated test relay and agent processes as uid
65534, with separate configuration, identities, credentials and project files
under one temporary directory. The relay listened only on loopback. A private
SSH tunnel connected the local MCP controller to it, and the wanctl device
transport was HTTP. SSH was used for fixture setup, test binary replacement
and cleanup; application development went through wanctl MCP exclusively.

The task was to implement a Python CSV order-summary CLI, preserving exact
money arithmetic and Chinese flavor names, ignoring refunded rows, and
reporting malformed paid rows without partial output. Codex read project
instructions and sample data, wrote code and tests, ran them, discovered an
unhandled malformed-CSV error, patched three locations in one edit request,
and reran the tests and CLI. The delivered project passes eight tests and its
sample reports six items and revenue `4.00`.

## Findings and changes

| Observation in the actual task | Cause | Change and recheck |
|---|---|---|
| Script exported `ORDER_TOOL_MODE`, next call returned `not-set` | Encoded script launched a child interpreter | New `exec_script` envelope verifies source against the encoded policy command, then executes matching-language source in the existing shell. Subsequent calls returned `immersion`; script `cd` also persisted. |
| A short command took roughly twice as long as a read | MCP started execution, waited locally, then connected again to poll | Device waits up to 250 ms for the job completion signal. Quick commands return their outcome on the original request; longer ones remain asynchronous. |
| Every operation still needed a workspace reference | Generic MCP has no reliable conversation-wide binding | Left explicit. An account-wide or transport-session-wide default would reintroduce cross-conversation state leakage. A host adapter must inject this per conversation. |
| Development MCP tools were absent from the current native tool catalog | The installed connector exposes the deployed version | Used a temporary protocol client. Native Codex tool redirection was not tested or claimed. |

The script authorization check runs on the device. A caller cannot authorize
one encoded script and supply different source for execution. A distinct
`exec_script` action makes older workspace agents reject the request rather
than silently run a child shell. Execution-mode changes also conflict with an
already used request ID. PowerShell normalization remains shared with the
ordinary encoded-script path; Windows execution was not part of this trial.

## Timing evidence

Five baseline short-exec calls had a median of **9,941 ms**. Five short-exec
calls after the fix had a median of **4,837 ms**. These are observations on this
one host and network route, not a general performance benchmark.

Two matching operations provide a narrower comparison:

| Operation | Before | After |
|---|---:|---:|
| Inspect environment and export a variable via a script | 8,966 ms | 4,159 ms |
| Read that variable on the next command | 9,869 ms | 4,453 ms |

The first attempted retest accidentally spawned the old MCP binary because
the temporary client's closure retained its earlier path. Checking the
spawned executable and its version exposed this. Those three calls are kept
in the raw journal but excluded from the after-fix sample. The corrected
client takes executable and environment explicitly.

## Continuity and exit

Codex started a delayed run of the actual project tests and then stopped both
the MCP subprocess and SSH tunnel. On reconnect, polling the original request
returned the complete eight-test result and exit code 0. Submitting the same
request ID and identical command returned the original result. A file written
by that command contained exactly one `completed` marker.

Explicit workspace exit returned `state=closed`. A later file read using that
reference failed with `workspace unavailable`; it did not select another
device. The test relay, agent, remote directory and temporary credentials were
then removed. The existing public deployment was not upgraded.

## Evidence and remaining work

Local ignored artifacts are retained under `artifacts/immersion-20260921/`:
the actual task source/tests in `orders/`, complete MCP call journal, separated
baseline/fixed call samples, `assessment.json`, cleanup confirmation and Go
check logs. Credentials are not retained. There were 33 MCP calls in total,
including setup reads, the application bug/fix, the mistaken-version retest,
disconnect recovery and exit checks.

Regression coverage adds persistent script cwd/env, source/policy mismatch
rejection, execution-mode ID conflicts and device-side bounded waiting to the
existing relay/MCP integration tests. Standard and lark test/vet/build checks,
targeted race detection, and Windows/Android cross-compilation accompany the
change; cross-compilation is not device acceptance.

The next product work should target the two remaining sources of friction:
conversation-scoped workspace injection in a concrete host, and authenticated
connection reuse with explicit revocation/reconnect semantics. This trial
does not establish native-tool transparency, broad model compatibility,
interactive PTY support, preview forwarding, or general model-efficiency gains.
