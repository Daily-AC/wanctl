# Harness parity with pi: write, batched edits, desktop capture, an output tail, and a system prompt

Wave 3 of turning wanctl into the external harness for a web AI. Five changes, all modelled
on what a local coding agent (pi 0.83) gives a model and wanctl did not.

## What changed

**1. `wanctl_write` / `wanctl write` — a third file primitive.** Read looks, edit patches,
write puts a whole file there. New protocol kind `file_write`, handled natively on the device
(`internal/server/fileops.go`), so there is no base64, no local temp file and no heredoc
through a shell. Parent directories are created; an existing file keeps its mode (a 0755
script stays executable), a new one gets 0644; the write is atomic through the same temp-file
+ rename path as edit; 8 MiB cap; content that is not UTF-8 is refused rather than written as
U+FFFD soup. Gated exactly like `file_put` (`policy.KindWrite`) and logged as a file event
carrying the path. The description carries pi's rule verbatim: *use write only for NEW files
or COMPLETE rewrites; for changes to an existing file use edit*.

**2. `wanctl_edit` takes a batch.** `edits: [{old, new}]` is an alternative to `old`/`new` —
exactly one of the two forms, mixing them is refused rather than resolved by precedence. Each
`old` matches the file as it was BEFORE any entry ran, must occur exactly once, and entries
may not overlap; any violation refuses the whole call by index (`edits[1] overlaps edits[0]`)
and leaves the file untouched. `all` belongs to the single form only; `expected_sha256` works
with both. The CLI keeps the single pair. The tool description carries pi's four guidelines:
one call with several entries, `old` as small as it can be while unique, no padding with
unchanged regions, every entry read against the original.

**3. `wanctl screenshot` works on desktops, and is an MCP tool.** macOS `screencapture -x`,
Windows the whole virtual screen (every monitor) through .NET, Linux grim → gnome-screenshot →
ImageMagick `import` with a refusal that names all three if none is installed. The Android path
is untouched. **The wire shape is unchanged**: it is still an exec of the verb `screenshot`
whose stdout is a PNG, so an existing controller keeps working when the device turns out to be
a laptop; the desktop capture is intercepted in the agent before the elevation channel
(`internal/agent/agent.go`), which a desktop does not have. The MCP tool returns the PNG as
image content plus one line with `WxH`, bytes and format; over 4 MiB it is downscaled and
re-encoded as JPEG q80 and the line says so.

**4. Over-long exec output keeps its tail, and the whole of it stays on the device.** The cap
is unchanged (48 KiB, `maxExecStream` in `internal/mcp/server.go`). What changed is which
48 KiB: the LAST ones, where a build's error and a script's result live, behind
`[output truncated: showing last X of Y bytes; full output at <path> on the device, kept for
1h …]`. The device writes that file itself (`internal/server/spill.go`) only once output
passes the threshold the controller named, so the bytes never cross the relay twice and a
short command leaves nothing behind. Expired spills are swept when the next one is created.
`exec_async` / `exec_poll` are untouched and still use the old head+tail clamp.

**5. MCP `instructions`.** The initialize response now carries the harness's system prompt,
built from the catalog (`internal/catalog/instructions.go`): what wanctl is, every primitive in
one list, the dev loop, the AGENTS.md rule, the four refusals. 39 lines. The same text is
`wanctl help --instructions` and a section of `docs/contract.md`, so nothing has a second copy.

## Criteria

| # | Criterion | Result |
|---|---|---|
| 1 | write: parents, 0644, mode kept, cap, non-UTF-8, policy, event | PASS — `internal/server/filewrite_test.go`; event `WRITE <path>` seen in the live device log |
| 2 | multi-edit: 3 applied, overlap/duplicate refused by index, forms exclusive, sha, CRLF | PASS — `internal/server/multiedit_test.go`, `internal/client/filewrite_test.go` |
| 3 | screenshot: real PNG here, 4 MiB+ downscaled, MCP image content | PASS — real capture **3456x2234, 1,105,496 bytes**; over the relay 3456x2234, 1,181,426 bytes |
| 4 | exec tail: last cap bytes, truncation line, device path holds everything | PASS — `internal/mcp/exectail_test.go`, `internal/client/execspill_test.go`; live: last 49,152 of 218,890 bytes |
| 5 | instructions ≤ 40 lines, complete, on the initialize response | PASS — 39 lines; `TestInitializeCarriesTheInstructions` does a real round trip |
| 6 | catalog: snapshot extended, must-keep extended, contract regenerated | PASS — with one deliberate schema change, below |
| 7 | gofmt / go vet / go test | PASS — all packages green; `TestAccessTokenFailsClosed` (#90) did not flake on either run |
| 8 | real link: relay + agent + controller, all four by hand | PASS — transcript below |
| 9 | a lost result is not an unsupported agent (write/edit/read/exec) | PASS — `TestLostConnectionAfterSendIsNotAnUnsupportedAgent`, `TestExplicitUnknownRequestIsStillAnUnsupportedAgent`, `TestFileErrorsDistinguishLostResultsFromOldAgents`, `TestLostExecConnectionIsNotAnUnsupportedAgent` |

### Criterion 8, by hand

`wanctl relay` on 127.0.0.1:18999, `wanctl agent --name lab-mac --mode bypass --yes`, the CLI
and a stdio MCP session as controllers.

```
$ wanctl write --target lab-mac …/work/conf/app.toml --content 'alpha = 1\nbeta = 2\ngamma = 3\n'
created …/work/conf/app.toml (29 bytes, sha256 fef954a8…)
-rw-r--r--  29  app.toml          # parent directory conf/ did not exist before

$ wanctl write --target lab-mac …/work/new/run.sh --content-file local.txt   # was chmod 755
overwrote …/work/new/run.sh (8 bytes, sha256 c3f9c8c2…)
-rwxr-xr-x  8  run.sh             # mode preserved

$ wanctl screenshot lab-mac -o shot.png
shot.png (1181919 bytes)   →  PNG image data, 3456 x 2234, 8-bit/color RGBA

wanctl_edit{…,"edits":[{"old":"alpha = 1","new":"alpha = 10"},{"old":"gamma = 3","new":"gamma = 30"}]}
  replaced 2 occurrence(s) … new sha256 d09b6b5b…, 31 bytes

wanctl_edit{…,"edits":[{"old":"beta = 2",…},{"old":"= 2",…}]}
  isError: edits[1] overlaps edits[0] in "…/app.toml"; nothing was written. Merge them into one entry
  (file on disk unchanged)

wanctl_exec{…,"command":"… 4000 echo lines …"}
  exit: 0
  [output truncated: showing last 49152 of 218890 bytes; full output at
   /var/folders/…/T/wanctl-exec-686df123c924.log on the device, kept for 1h — read it with
   wanctl_read or grep it with wanctl_exec]
  …ends at "line 3999: the quick brown fox jumps over the lazy dog"
  $ wc -c wanctl-exec-686df123c924.log → 218890   (head: "line 0:", tail: "line 3999:")

wanctl_screenshot{"target":"lab-mac"}
  text:  3456x2234, 1183087 bytes, image/png
  image: image/png, 1577452 base64 chars

initialize → instructions: 39 lines
device event log: WRITE …/app.toml · EDIT …/app.toml · WRITE …/run.sh, all with decisions
```

## Criterion 9, added after review of the WebFetch PR

`internal/client/fileops.go` turned any `io.EOF` after the request frame into
`UnsupportedError` — "device agent does not support …; run `wanctl update`". That inference is
only sound when the device says so. A device that had already applied an edit and then lost the
connection produced a message claiming nothing ran, naming a fix that would not help, and
inviting a retry that for a non-idempotent edit replaces text that is no longer there.

Now only an explicit `unknown request: <kind>` reply means unsupported. A connection that ends
after the frame was sent is a new `ResultLostError` carrying the kind and the path:

> result unknown: the connection dropped after the request was sent; read the file and compare
> sha256 before retrying — the change to /etc/app.conf may or may not have been applied

`mcpRead`, `mcpEdit` and `mcpWrite` all render it through `fileOpErrorResult`, which adds
"Do NOT simply retry: call wanctl_read first and compare the sha256", and never the update
instruction. A lost `file_read` says the opposite — nothing was changed, so retry — because a
read has nothing to inspect afterwards. Both failures are now rows in the three tools' error
tables in the contract.

The exec path (which is how a screenshot travels) never had the bug: it returns the read error
as itself. `execOver` was extracted from `ExecOut`, mirroring the `fileOpOver` seam, so that
property is asserted rather than assumed.

## Review round 2

Seven findings. One accepted as an owner decision, six fixed.

1. **A desktop capture is auto-approved under bypass.** Accepted: it is gated as an ordinary
   command, and bypass auto-approves ordinary commands. Now said plainly in the tool
   description rather than left to be discovered.
2. **The batch built its result before checking the projected size.** The single-edit path has
   always sized first, deliberately; the batch allocated the whole oversized string and only
   then refused. `applyEdits` now sums `len(new)-len(old)` over the matched spans and refuses
   above the cap before `strings.Builder` sees anything.
3. **No cap on the number of edits.** Each entry scans the whole file, so one small frame could
   spend a device's CPU. Capped at `protocol.MaxBatchEdits` = 64, refused before the file is
   opened, and in the contract.
4. **Spill write and open errors were discarded.** A full disk produced a truncated file
   advertised as the complete output — the worst kind of wrong, because a caller greps it and
   believes the absence of a match — and a failed open re-entered the branch, running a Glob
   per chunk. The writer now latches the failure on the first error, removes the partial file,
   and reports no path. The controller says "the device could not keep the full output", which
   it can tell from "the agent is too old" because the byte count is reported either way.
5. **Spill growth.** Each file is capped at 8 MiB (it keeps the FIRST 8 MiB; the controller is
   already showing the last, so both ends are covered and the caller is told the middle is
   gone) and the retained count at 32, oldest evicted first, swept on every open and once at
   agent start.
6. **A lost read was told to go hash a file it never changed.** The appended
   "Do NOT simply retry" clause is now skipped for `file_read`, whose message already says
   retrying is safe.
7. **The spill path was dropped on an error frame.** A command that failed after emitting
   megabytes is exactly when it matters. `ExecOutcome` now carries path, total and kept bytes
   out of the error path too, and `mcpExec` appends the note to the failure.

| Finding | Test |
|---|---|
| 2 | `TestMultiEditRefusesAGrowthOverTheLimitWithoutBuildingIt` |
| 3 | `TestMultiEditRefusesMoreEntriesThanTheCap` (over, and exactly at, the cap) |
| 4 | `TestSpillThatCannotBeWrittenReportsNoPath`, `TestFailedSpillDoesNotRetryPerChunk` |
| 5 | `TestSpillStopsAtTheSizeCap`, `TestSweepCapsTheNumberOfRetainedSpills` |
| 6 | `TestALostReadIsNotToldToCheckTheFile` |
| 7 | `TestSpillSurvivesAnErrorFrame`, `TestTruncationSaysWhichSilenceThisIs` |

Addendum, four more:

8. **"Run wanctl update" was printed for any empty spill path.** Already fixed with item 4 and
   kept: the device reports the byte count whether or not it could keep the output, so a
   current agent that failed says "could not keep the full output" and only an agent that
   reported nothing at all is told to update. `TestTruncationSaysWhichSilenceThisIs` covers
   both renderings.
9. **The truncation line counted the local buffer.** The device counted every byte it
   produced; this side holds only what arrived on one stream. The device's number is used when
   it is the larger. `TestTruncationReportsTheDeviceByteCount`.
10. **Only an EOF became a lost result.** A reset or a timeout after the request frame is
    exactly as unknown, and came back as a bare transport error a caller reads as "it failed,
    so retry". Every post-send failure is now a `ResultLostError`, carrying its cause so
    `errors.Is` still finds the timeout and the message names the reset.
    `TestAnyPostSendFailureIsALostResult`.
12. **The contract hardcoded "array of {old, new}".** True only while `edits` was the only
    array in the catalog. The label is rendered from the parameter's own item schema, in the
    order the schema declares the fields required.
    `TestArrayParameterRendersItsDeclaredItemShape`.

## Contradictions and judgment calls

**`wanctl_edit`'s `old` and `new` are no longer `required` in the MCP schema.** The brief said
existing tools' registration must not move, and the same brief said edit gains `edits` as an
alternative to `old`/`new`. Both cannot hold: a schema that demands `old` makes the alternative
unusable in any host that enforces `required`. `edits` was added and the two flags relaxed to
optional, which is backward compatible for every existing caller (they still pass old/new) and
is the only shape in which the new form works. The snapshot fixture records it; nothing else
about the existing tools moved.

**Criterion 4 needed a protocol field.** The exec cap lives controller-side
(`internal/mcp/server.go`), but "the full output is on the device" cannot be true unless the
device writes it. The alternatives were pushing the bytes back up the relay (doubling traffic
on exactly the commands that are already too big, and needing a write grant) or teeing every
exec on every device unasked. Instead `exec` carries an optional `spill_after`, and the exit
message carries the path and the true byte count. This is the same additive mechanism `elevate`
uses: an older agent decodes the field into nothing, answers with no path, and the truncation
line then says the output was not kept and to run `wanctl update`. The brief's blocked-protocol
clause is about being forced into a breaking redesign; an optional JSON field on an existing
kind is not one, so this was implemented rather than reported as blocked. Flagging it because
the call was mine.

**A desktop screenshot is gated exactly like a command; Android stays elevated.** The first
version of this branch gated every capture as `policy.KindExecElevated`, the class the Android
path uses. Reviewed and changed: on a laptop a capture needs no privilege the exec gate does
not already grant — a controller allowed to run commands can run `screencapture` itself — so
the stricter gate only added friction to the see-the-screen loop and created a second, harder
path to the same capability. The device decides, because only the device knows which kind it
is: `server.IsDesktopCapture` in `internal/agent/agent.go` picks `KindExec` for the verb on a
non-Android device and leaves Android on `KindExecElevated`, where su or adb is genuinely
required. The wire is unchanged — a capture is still requested elevated, since the controller
cannot know what will answer — and `ElevateOptional` keeps the "the device did not elevate
this command" check from firing on a laptop's reply that names no channel.

**One index line was spent.** `wanctl` bare prints a 30-line budget and `write` needed a row,
so `agent` (the foreground spelling of `start`, next to `start`/`stop`/`status`/`service`) moved
off the index to the MORE line. It keeps its full help entry.

**`--via` is now accepted by the MCP screenshot tool** and ignored by a desktop, because the
same tool serves both and a caller driving an Android device should not need a second one.
