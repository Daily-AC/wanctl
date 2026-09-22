# MCP structured results

The development-loop tools advertise `outputSchema` in `tools/list` and return
matching data in `structuredContent`. Existing `content` text is preserved for
clients that still consume it. CLI output, tool names, input arguments and
device permissions are unchanged.

| Tool | Structured result |
| --- | --- |
| `wanctl_workspace` | Workspace reference/root/state, plus an inspected command when requested |
| `wanctl_peers` | Devices, aliases, trust state and shared-device targets |
| `wanctl_exec` | Workspace command result, or ordinary command exit code and bounded stdout/stderr |
| `wanctl_exec_async` | Workspace command result, or an accepted background job ID |
| `wanctl_exec_poll` | Workspace command page, or ordinary job state/output/next byte offset |
| `wanctl_read` | File text, line range, whole-file size/hash, truncation and continuation metadata |
| `wanctl_write` | Created versus overwritten, resulting size and hash |
| `wanctl_edit` | Replacement count, resulting size and hash |

Execution tools have two documented result forms because ordinary device jobs
and workspace commands have different lifetimes. Their schemas use `anyOf`;
the presence of a `workspace` reference identifies the workspace form. An
ordinary asynchronous submission returns a job ID, not a guessed running or
finished state. Poll that ID to observe its state. Other tools do not advertise
an output schema in this change.

## Lifecycle state and command completion

For a workspace lifecycle operation, inspect `state`. A successful `exit`
returns `state="closed"`, with no `request_id`, `done=false` and a placeholder
`code=0`. That `done=false` does not mean that closing failed.

For an inspected command, `request_id` identifies the command and `done`
reports its completion. Interpret `code` only once that command is done. An
empty output page does not mean a command has finished. Even after completion,
continue polling while `next_offset < retained_bytes`.

## Output limits and file hashes

Workspace output uses byte offsets and distinguishes produced bytes from
retained bytes. Ordinary execution retains its existing display limits and
truncation notices; its structured stdout/stderr/output fields contain the
same bounded display text. A reported `spill_path` is on the device and exists
only when the device reports a copy. The schema does not promise a full log
when no such copy exists.

Devices can report `spill_bytes` even for short output that needed no copy.
Only `spill_path` identifies an actual reported copy; a zero `spill_kept`
without a path does not by itself imply output was lost.

Ordinary device shell execution merges stdout and stderr into the stdout
channel. Separate stderr metadata describes only separate frames actually
received; an empty stderr field does not mean the command produced no errors.

File reads use **line** offsets. The hash and size describe the **whole file**,
not only the returned range. A read's hash can be passed directly to edit's
`expected_sha256`. When a byte cap cuts a read at a whole-line boundary,
`next_offset` supplies the next line. A nonzero `long_line` means one line alone
exceeds the cap; in that case no `next_offset` is promised.

## Errors and validation

Pre-execution failures retain the existing `isError=true` and diagnostic text,
including pairing URLs, identity mismatches and uncertain-write guidance. They
need not fabricate a successful result object. Workspace command failures can
also carry structured command results alongside `isError=true`. Ordinary exec
and poll preserve their existing handling of nonzero command exit codes; read
`code` instead of treating the absence of `isError` as command success.

The schema source is `internal/catalog/output.go`, used by both stdio and HTTP
registration. The server enables the SDK's output validation. Tests additionally
compile the schemas received over `tools/list` and validate actual tool results
from a real relay, device agent, shell and filesystem, including empty results,
running jobs, truncation and failures. This explicit check also catches absent
structured data, which the SDK's compatibility mode does not reject.

Output schemas describe results; they do not grant permission or replace host
approvals. The existing workspace and device authorization checks still apply.
