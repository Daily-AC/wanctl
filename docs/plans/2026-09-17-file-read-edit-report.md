# Native `read` and `edit` for remote files

2026-09-17 · branch `feat/file-read-edit`

## What this adds

Two first-class file operations that do not go through a shell, on three surfaces:

| Surface | Read | Edit |
| --- | --- | --- |
| Agent protocol | `file_read` | `file_edit` |
| MCP (stdio **and** hosted HTTP) | `wanctl_read` | `wanctl_edit` |
| CLI | `wanctl read` | `wanctl edit` |

Before this, the only way for an AI driving wanctl to look at or change a remote file was
`exec` with `cat`/`sed`/`echo`, or a whole-file `push_blob`. That is four different programs
across the four platforms an agent runs on, the caller's text is shell source before it is
data, output is cut off by whatever the caller remembered to pipe it through, and a "patch"
is really an overwrite that discards anything written since the read. Both new operations
are pure Go on the device, so they behave identically on Linux, macOS, Windows and Android.

## Files changed

| File | Change |
| --- | --- |
| `internal/protocol/protocol.go` | `KindFileRead` / `KindFileEdit` / `KindFileResult`; `Old`/`New`/`All`/`ExpectedSHA`/`File` message fields; `FileResult`; the `MaxReadBytes` (256 KiB), `MaxEditBytes` (8 MiB), `DefaultReadLines` (2000) limits |
| `internal/server/fileops.go` | new. `HandleFileRead` / `HandleFileEdit`, the streaming line collector, the text sniff, the atomic edit |
| `internal/agent/agent.go` | two dispatch cases in `serveAuthorized`, gated and logged like `file_get`/`file_put`; `requiredCapability` maps the new kinds to `sessionauth.Read`/`Write` |
| `internal/client/fileops.go` | new. `Client.ReadFile` / `Client.EditFile`, the shared `fileOpOver` exchange, `UnsupportedError`, `FileOpError` |
| `internal/mcp/server.go` | `wanctl_read` / `wanctl_edit` registration and handlers, `fileOpErrorResult`; one pointer sentence added to each of `wanctl_exec`, `wanctl_push`, `wanctl_push_blob`, `wanctl_pull` |
| `main.go` | `read` / `edit` subcommands, `parseAroundPositionals`, `editText`, usage entries, `relayCommands` entries |
| tests | `internal/server/fileops_test.go`, `internal/agent/fileops_test.go`, `internal/client/fileops_test.go`, `internal/mcp/fileops_test.go`, `fileops_cli_test.go` |

Both surfaces share one client function per operation: the CLI, the stdio MCP server and the
hosted HTTP MCP server all call `Client.ReadFile` / `Client.EditFile`.

`wanctl_read` and `wanctl_edit` are available in hosted HTTP mode, unlike `wanctl_push` and
`wanctl_pull`. Those two are stdio-only because they name a path on the MCP server's own
disk, which on a shared server belongs to someone else. Read and edit name a path on the
target device, which is the thing the caller was authorized to drive in the first place.

## Old-agent behaviour (what was found)

`internal/agent/agent.go:812` — the request loop's `default:` branch:

```go
default:
    protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindError, Reason: "unknown request: " + m.Kind})
    return
```

An agent that predates these kinds **answers and then ends the session**; it does not drop
the frame silently. So no controller-side timeout is needed, and none was added: the
controller recognizes the `unknown request` prefix and returns `UnsupportedError`, whose
message is *"device agent <version> does not support read/edit; run `wanctl update` on the
device"*. The same precedent already exists for the `status` verb
(`internal/client/status.go:50`).

The version in that message is a best-effort second dial (`Client.Status`) on a path that has
already failed, bounded by its own 5 s budget, so naming the version can never be what makes
the caller wait. The message reads correctly without it.

This is verified two ways: against a stand-in device that replies exactly as the branch above
does, and against a **live agent** sent a frame kind it has never heard of.

## Criteria

| # | Criterion | Result | Where |
| --- | --- | --- | --- |
| 1 | read 5000-line file, offset 4990 limit 20 → lines 4990–5000, total 5000, truncated false | PASS | `TestReadReturnsTheRequestedLineRange` |
| 2 | read with no limit on a 1 MiB wide file → truncated=true, ≤ 256 KiB | PASS | `TestReadStopsAtTheByteCap` |
| 3 | read of a file with NUL bytes → "not text" refusal | PASS | `TestReadRefusesBinaryFiles` |
| 4 | `old` twice, `all` false → refused, count 2, file and sha unchanged | PASS | `TestEditRefusesAmbiguousMatch` |
| 5 | wrong `expected_sha256` → refused, response carries actual sha, file unchanged | PASS | `TestEditRefusesOnHashMismatch` |
| 6 | CRLF file, mode 0755 → CRLF and mode preserved, only the replaced span differs | PASS | `TestEditPreservesBytesAndMode` |
| 7 | `all` true replaces every occurrence and reports the count | PASS | `TestEditAllReplacesEveryOccurrence` |
| 8 | edit gated like `file_put`, read like `file_get` | PASS | `TestFileReadAndEditUseTheSamePolicyKindsAsGetAndPut`, `TestFileReadAndEditRefusalsMatchGetAndPut`, `TestBypassModeCoversEdit` |
| 9 | old agent → the update instruction, within 10 s | PASS | `TestOldAgentUnknownKindBecomesAnUpdateInstruction`, `TestLiveAgentUnknownKindBecomesAnUpdateInstruction` |
| 10 | real link: build + local agent/controller round trip | PASS | see below |
| 11 | `gofmt -l .` prints nothing | PASS | clean |

`go vet ./...` clean, `go test ./...` green across all 30 packages (no rerun needed; the known
`TestAccessTokenFailsClosed` flake, issue #90, did not fire).

### Criterion 10, the real link

Two harnesses, both real:

1. **In-process**, the fixture `TestClientExecAndFileRoundTrip` already uses — an `httptest`
   relay, a real `agent.Agent`, a real `client.Client` over TLS. Reused by
   `TestReadEditRoundTrip`, which reads a range, edits against the hash it just read, reads
   the change back, and confirms a stale hash is refused end to end. No shell script that
   spins relay+agent locally exists in `scripts/`, `tools/` or `selfhost/`.
2. **The built binary**, by hand on this Mac: `wanctl relay` on 127.0.0.1:18999, `wanctl agent
   --name lab-mac --mode bypass --yes`, and the CLI as a third process. Against a 5000-line
   CRLF file at mode 0755:
   - `wanctl read … --offset 4990 --limit 20` returned 11 CRLF lines with the trailer
     `lines 4990-5000 of 5000, sha256 3cfd6b88…, truncated=no`, whose hash matched the local
     `shasum -a 256` of the whole file.
   - `wanctl read` with no flags returned lines 1–2000 of 5000.
   - `wanctl edit --sha <that hash>` printed `replaced 1 occurrence(s), sha256 d3dbeb30…`;
     afterwards the file was still mode 0755 with all 5000 CRLF endings intact.
   - Re-running the edit with the now-stale `--sha` was refused, exit 1, the message carrying
     the file's current hash; an edit whose `old` matched 5000 times was refused, exit 1.
   - Reading a binary file was refused with the not-text message, exit 1.
   - `wanctl logs --type file` showed `READ <path>` and `EDIT <path>` entries with decisions.

## Deliberate decisions

- **No controller-side timeout for the two ops.** The spec allowed one only if the old agent
  dropped unknown kinds silently. It does not (see above), and a blanket 10 s deadline would
  have broken the approval path: on a device in normal mode, `gateFile` blocks while a human
  decides, which legitimately takes minutes.
- **`wanctl read`/`wanctl edit` accept flags on either side of the path.** Go's `flag` package
  stops at the first non-flag argument, so `wanctl read /path --limit 20` — the order a person
  types, and the order the specified usage line shows — would have silently ignored the limit.
  `parseAroundPositionals` resumes parsing after each positional, so a flag's own value is
  never mistaken for one.
- **The edit writes through `newPendingUpload`**, the same temp-file-plus-rename under an
  `os.Root` that `file_put` uses, so the policy decision constrains the open itself and a
  symlink swapped in mid-operation cannot redirect the write.

## Left out

- `internal/webfetch` and `internal/portal` were not touched; WebFetch gets these operations
  in a later wave, per the task boundaries.
- No help-catalog refactor: the two new commands were added in the same style as their
  neighbours, and no existing tool description was rewritten beyond one added sentence each
  pointing at read/edit.
- Nothing was run against a real remote device (Windows "zyl", the Linux server). That
  acceptance pass belongs to the commander after review.
