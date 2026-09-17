# WebFetch read and edit — paging a file, and patching one span of it

Branch `feat/webfetch-read-edit`, 2026-09-17, on top of #91 (native `file_read` /
`file_edit`) and #92 (WebFetch for strong models).

A web chat AI could already run a command and move a whole small file. It could
not look at line 4,000 of a log, and the only way it had to change a file was to
overwrite the whole thing from a 2 KiB parameter. This exposes the two device
operations that fix that — a paged read and an exact-string edit — as GET tools
on the same protocol.

## What changed

| File | What |
| --- | --- |
| `internal/webfetch/handler.go` | `read_text` gains `offset`/`limit` and goes through `client.ReadFile`; new `edit_text` through `client.EditFile`; `Operation` gains five optional fields that stay out of the canonical payload when omitted; `fitRead` clamps a device read to the 32 KiB response cap on a line boundary; a device-side refusal is now a decided outcome (`file_refused`) rather than `unknown` |
| `internal/webfetch/manifest.go` | Four tools instead of three; `input_schema` and `call_url_template` for both file tools; descriptions rewritten as operating instructions; `read_lines_default` and `write_bytes_max` in `limits`; the new-rid security rule now names `file_refused` |
| `internal/webfetch/fileops_e2e_test.go` | New. Paging, the oversized line, the edit happy path and its four refusals, and the write gate |
| `internal/webfetch/discovery_test.go` | Manifest contract for both tools plus the page budgets; replay identity for `offset`/`limit`/`all`/`expected_sha256` |
| `internal/portal/web/webfetch-help.html` | Two new table rows and a paragraph on read-vs-exec and edit-vs-write |
| `docs/webfetch.md`, `docs/webfetch.zh.md` | Tool table, a section on what the two operations are and how to page, the limits paragraph, the new-rid rule |

`docs/portal/ai__*` does not list the WebFetch tools, so it is untouched.

## The result shapes

`read_text` returns `content`, `total_lines`, `first_line`, `last_line`,
`size_bytes`, `sha256` (of the whole file), `truncated`, and then `next_offset`
only when the byte cap cut the range on a line boundary, or `long_line` only
when one line cannot fit at all. `edit_text` returns `replaced`, `sha256` and
`size_bytes`; a refusal carries `error_code: "file_refused"`,
`execution_started: false`, the file's actual `sha256`, its `size_bytes` and,
for an ambiguous `old`, `occurrences`.

Two deliberate calls, both worth arguing with:

**`read_text` no longer returns `byte_count`.** It used to mean the bytes of the
whole downloaded file, which was the same as the content. Now content is a
window and the file's size has its own field, so `byte_count` would have been a
third number meaning neither. `write_text` keeps it.

**A refused read or edit says `execution_started: false`.** Before this change
the only results carrying that flag were `pairing_required` and `adapter_busy`,
and the security rules say a new rid is correct only after one of them. An edit
refusal has no way forward otherwise: the same rid with corrected arguments is a
409 by design, and a new rid after a plain `failed` is forbidden. The device
decided and wrote nothing, so this is the same kind of answer, and the rule in
`securityRules()` now names all three.

## Criteria

| # | Criterion | Result |
| --- | --- | --- |
| 1 | Read with `offset`/`limit`, continuation, `long_line`, non-text refused | `TestWebFetchReadTextPagesWithoutLossOrRepeat` walks a 120-line file in windows of ten and reassembles it byte for byte, then has a NUL-byte file refused; `TestWebFetchReadTextNamesALineTooLargeToPage` builds a line of `MaxOutputBytes+4096` and asserts `long_line`, no `next_offset`, and that the lines on either side still read |
| 2 | Edit happy path and its refusals on a CRLF file | `TestWebFetchEditTextAppliesOneSpanAndRefusesTheRest`: hash-guarded replace, ambiguous `old` refused with `occurrences: 2`, stale `expected_sha256` refused with the real sha, `all=true` replacing both, an explicit empty `new` deleting a line, and an omitted `new` rejected at 400. Every step re-reads the file from disk and compares exact bytes, CRLF included |
| 3 | Omitted parameters stay out of the rid hash; the same URL replays | `TestOmittedFileParametersAreNotPartOfTheReplayIdentity` (all four parameters, plus `all=false` hashing like no `all`); the replay half is in the paging test — the identical read URL returns `duplicate_request: true` and the same `job_id` |
| 4 | An unruled path sends `edit_text` down the write gate | `TestWebFetchEditIsGatedAsAWriteNotAsARead`. See the caveat below |
| 5 | Manifest contract and the page budget | `TestManifestPublishesBothFileToolContracts`: schemas, required lists, the `{old}`/`{new}` template, the phrases each description has to teach, and both rendered pages against their budgets |
| 6 | `gofmt`, `go vet ./...`, `go test ./...` | All clean, with PostgreSQL actually running. `internal/webfetch`: 27 tests pass, 0 skipped |
| 7 | Real read and edit responses | Below |

### How PostgreSQL was run

```sh
docker run -d --name wanctl-wf-pg -e POSTGRES_PASSWORD=wanctl -e POSTGRES_USER=wanctl \
  -e POSTGRES_DB=wanctl -p 55433:5432 postgres:16-alpine
WANCTL_TEST_POSTGRES="postgres://wanctl:wanctl@127.0.0.1:55433/wanctl?sslmode=disable" \
  go test -count=1 ./...
```

`go test -count=1 -v ./internal/webfetch/` reports 27 `--- PASS` and zero
`--- SKIP`, so the end-to-end tests really ran rather than skipping past a
missing `WANCTL_TEST_POSTGRES`.

### Criterion 4, honestly

The criterion says "the same pending-approval path as `write_text`". The gate
itself lives in `internal/agent/agent.go`, which this branch may not touch, and
it already routes `file_edit` through `policy.KindWrite`; `internal/agent/
fileops_test.go` from #91 asserts that and the `EDIT <path>` audit line.

What a WebFetch-level test can show is the behaviour on the far side of it. The
end-to-end fixture is headless — no console front-end subscribes — so the
approval queue's answer to an unattended request is an immediate denial rather
than a wait. The test therefore asserts on a directory with no rule that
`edit_text` and `write_text` come back with the *identical* error string,
`write denied by device policy`, while `read_text` on the same path says
`read denied`, and that the same edit applies in the directory where the owner
did approve writes. Same gate, same grant, different from a read. A test that
proved a human-visible pending card would have to drive the console service,
which is a different package's contract.

## Real run

`go run ./tools/webfetch-demo` against the same PostgreSQL, relay and portal on
loopback, one real agent, device policy `normal` with read/write on the sandbox
directory only. The delegation was created by fetching `/webfetch/new/{nonce}`
and approved through the portal's `/api/delegations/approve`. The file was
`config.ini`, written with CRLF endings.

The first call came back at the pairing checkpoint, unchanged from #92 and
correct for a file tool too:

```json
{"ok": false, "tool": "read_text", "error_code": "pairing_required",
 "execution_started": false,
 "pairing_url": "http://127.0.0.1:18996/#pair?device=5b22b002-…&fp=SHA256%3AXw0JKqxr…"}
```

Pairing was then recorded in the device's own `known_clients.json`, the way the
end-to-end fixture does it; the portal's `/api/devices/pair` returned 200 but did
not record the decision, which is pre-existing demo-fixture behaviour and outside
this branch.

`GET …/call?rid=read-live-1&tool=read_text&target=…&path=…&offset=1&limit=2&format=json`:

```json
{
  "status": "done",
  "job_id": "j_ac3f65e029bd8554b9b13e2e0ab2a295",
  "request_id": "read-live-1",
  "result": {
    "ok": true,
    "tool": "read_text",
    "target": "webfetch-demo/5b22b002-a587-4a0b-8acc-abb3d3f20f84",
    "content": "alpha = 1\r\nbeta = 2\r\n",
    "first_line": 1,
    "last_line": 2,
    "total_lines": 3,
    "size_bytes": 32,
    "sha256": "b8374a5fb879bde72fd05e4cac18945b42c458d2388aa90dd678833b5bfd6e32",
    "truncated": false
  }
}
```

`GET …/call?rid=edit-live-1&tool=edit_text&…&old=beta%20%3D%202&new=beta%20%3D%2022%20%20%23%20%E6%94%B9%E8%BF%87%E4%BA%86&expected_sha256=b8374a5f…`:

```json
{
  "status": "done",
  "job_id": "j_ddde1270327520f513661ba9f89d7d27",
  "request_id": "edit-live-1",
  "result": {
    "ok": true,
    "tool": "edit_text",
    "target": "webfetch-demo/5b22b002-a587-4a0b-8acc-abb3d3f20f84",
    "replaced": 1,
    "size_bytes": 46,
    "sha256": "56aa5aa13f991b975f7ad476aee2460adacd0e5597ab286f426b6bf5c666d9ff"
  }
}
```

`od -c` on the device's file afterwards, with the CRLF on both sides of the
edited span intact and the non-ASCII replacement arriving literally:

```text
0000000    a   l   p   h   a       =       1  \r  \n   b   e   t   a
0000020    =       2   2           #      改  **  **  过  **  **  了  **
0000040   **  \r  \n   g   a   m   m   a       =       3  \r  \n
```

And a refusal, with the sha the caller needs to recover:

```json
{
  "status": "failed",
  "result": {
    "ok": false, "tool": "edit_text",
    "error": "\"…/config.ini\" changed since it was read: expected sha256 0000…0000, found 56aa5aa1…. Nothing was written; read the file again and redo the edit against its current text",
    "error_code": "file_refused",
    "execution_started": false,
    "sha256": "56aa5aa13f991b975f7ad476aee2460adacd0e5597ab286f426b6bf5c666d9ff",
    "size_bytes": 46
  }
}
```

## Contradictions and judgement calls

- **`next_offset` only when `truncated`.** The brief defines it that way and it
  matches the native operation, but it means a caller that sets `limit=10` on a
  100-line file gets `last_line: 10`, `total_lines: 100` and no `next_offset`.
  The tool description carries the general rule instead: continue at
  `last_line + 1` while `last_line` is below `total_lines`. Left as specified.
- **`long_line` is reported at two different caps.** The device names a line over
  256 KiB; WebFetch names a line that will not fit its own 32 KiB response, which
  is the cap that actually binds here. Both mean the same thing to the caller —
  do not page on, use `exec` — so they share the field.
- **A new error code.** `file_refused` is the first `error_code` this protocol
  has added since #92. It could have been two codes, one per tool; one concept
  ("the device decided and changed nothing") reads better in the security rule.
- **`all` accepts only `true` or `false`.** `strconv.ParseBool` would also take
  `1`, `t`, `TRUE`. Strict values give a model a clear 400 instead of silent
  acceptance, and keep one canonical spelling per operation.
- **`expected_sha256` is not case-normalised.** The device compares case-
  insensitively, but the canonical payload keeps what was sent, so the same rid
  with a differently-cased sha is a 409. Every sha a caller has comes from a
  `read_text` result, which is lowercase.
