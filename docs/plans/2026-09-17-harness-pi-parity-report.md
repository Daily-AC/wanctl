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

**A desktop screenshot is gated as an ELEVATED command.** The brief said to pick the gate the
Android path already uses, which is `policy.KindExecElevated`. Kept on every platform, and it
has teeth: bypass mode deliberately does not cover elevated, so a device in bypass still
refuses a capture until its owner grants the rule — which is the right answer for looking at
someone's screen, but it is stricter than `exec` and the tool description says so. Because
nothing is actually elevated on a laptop, the controller carries an `ElevateOptional` flag so
the "the device did not elevate this command" check does not fire on a reply that names no
channel. This is controller-local; the wire is unchanged.

**One index line was spent.** `wanctl` bare prints a 30-line budget and `write` needed a row,
so `agent` (the foreground spelling of `start`, next to `start`/`stop`/`status`/`service`) moved
off the index to the MORE line. It keeps its full help entry.

**`--via` is now accepted by the MCP screenshot tool** and ignored by a desktop, because the
same tool serves both and a caller driving an Android device should not need a second one.
