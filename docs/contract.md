# wanctl command contract

wanctl is a remote computer for AI agents. A trust layer — relay, pairing,
pinned device identity, device-side policy rules — decides who may drive which
machine, and on top of it sits a deliberately small set of primitives: run a
command, run a background job, read a file, patch a file, move bytes, read the
log. There is no IDE, no browser driver and no second way to do any of these;
anything richer is built out of them by the agent.

This file is generated. It is the output of `wanctl help --markdown`, and
the same catalog (`internal/catalog`) produces the CLI help and the MCP tool
descriptions, so the three cannot drift. Regenerate with:

```
go run . help --markdown > docs/contract.md
```

## Commands

| Command | MCP tool | Summary |
|---|---|---|
| `login` | `wanctl_login` | Authenticate to a wanctl namespace through the portal |
| `status` | `wanctl_status` | Report login state, and on a device the agent's mode and version |
| `logout` | `wanctl_logout` | Clear the stored credentials |
| `peers` | `wanctl_peers` | List reachable devices and whether their identity is pinned |
| `pair` | `wanctl_pair` | Check a device's trust state, or get the URL to pair with it |
| `exec` | `wanctl_exec` | Run a command, or a whole script, on a device |
| `read` | `wanctl_read` | Read a range of lines from a text file on a device |
| `edit` | `wanctl_edit` | Replace a string inside a file on a device, atomically |
| — | `wanctl_exec_async` | Start a background job and return its id at once |
| — | `wanctl_exec_poll` | Fetch a background job's new output and status |
| `push` | `wanctl_push` | Upload a local file to a path on the device |
| — | `wanctl_push_blob` | Upload inline base64 content to a path on the device |
| `pull` | `wanctl_pull` | Download a file from the device to a local path |
| `logs` | `wanctl_logs` | Read a device's activity log: connects, execs, file operations |
| `logs --service` | `wanctl_server_logs` | Read recent portal or relay process logs |
| `id` | `wanctl_id` | Show this controller identity's fingerprint |
| `trust` | `wanctl_trust` | List the trust store |
| `trust server` | `wanctl_trust_server` | Pin a device's identity for this controller |
| `rules` | `wanctl_rules` | List the policy rules this machine enforces on controllers |
| `start` | — | Turn this machine into a controlled device |
| `stop` | — | Stop the agent started by `wanctl start` |
| `service` | — | Install, remove or inspect an OS-native always-on service |
| `agent` | — | Run the agent in the foreground |
| `update` | — | Replace this binary with the latest signed release |
| `version` | — | Print the release version |
| `mcp` | — | Run wanctl as an MCP server |
| `docs` | — | Read and write the portal's documentation articles |
| `friends` | — | List and manage friend relationships between namespaces |
| `share` | — | Grant another namespace the use of one of your devices |
| `screenshot` | — | Capture an Android device's screen to a local PNG |
| `config` | — | Show or persist relay, portal and transport settings |
| `label` | — | Show or set this controller's self-description |
| `admin` | — | Mint, list and revoke admission invites |
| `portal-admins` | — | Manage the local portal root fingerprints |
| `relay` | — | Run the relay (the public broker) |
| `portal` | — | Run the web portal |
| `help` | — | Print this contract |

## `wanctl login` / `wanctl_login`

*Authenticate to a wanctl namespace through the portal*

Authenticate THIS MCP session to a wanctl namespace via the team portal.
Two-step OAuth flow: (1) call with NO argument first → returns a portal URL +
a one-time code prompt the user needs to complete in their browser. (2) call
again with the `code` the user pastes back → exchanges it for a namespace
token bound ONLY to this MCP session (in HTTP mode) or this machine's wanctl
config (in stdio mode). Multiple AI users sharing the same MCP server each log
in independently — credentials are never shared across sessions.

FAST RE-BIND: a successful login also returns a `rebind` credential. HTTP-MCP
sessions are in-memory, so a relay restart or a dropped/re-initialized
connection can surface 'LOGIN REQUIRED' mid-task even though the user is still
authorized. When that happens, call wanctl_login(rebind="…") with the
credential you saved — it restores access INSTANTLY with no portal round-trip.
Only fall back to the OAuth flow if you have no saved rebind credential.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| `code` | `--code CODE` | string | no | The one-time code the user copied from the portal /enroll page. Omit on the first call. |
| `rebind` | — | string | no | A rebind credential returned by an earlier successful login in this conversation. Pass it to restore a lost session instantly without re-doing OAuth. Mutually exclusive with code. |

```
wanctl login [--code CODE]
```

```
wanctl_login{}  then  wanctl_login{"code":"ABC123"}
```

| Error | What to do |
|---|---|
| `LOGIN REQUIRED` | No usable credential. Run `wanctl login` (CLI) or wanctl_login (MCP); an MCP session that had one can restore it instantly with wanctl_login(rebind=…). |

## `wanctl status` / `wanctl_status`

*Report login state, and on a device the agent's mode and version*

Report whether this MCP session is logged in, what namespace it's bound to,
and the controller fingerprint. Call this if a tool says 'login required' and
you're not sure if a login already completed.

**On the command line.**

Without --target this reads local state only and dials nothing. With one it
asks that device for its agent mode and version, which is the quickest check
that a device is reachable at all.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| — | `--target NS/DEV` | string | no | Ask this device for its agent mode and version instead of reporting local state. Omit it to read this machine only, which dials nothing. |

```
wanctl status [--target NS/DEV]
```

```
wanctl_status{}
```

## `wanctl logout` / `wanctl_logout`

*Clear the stored credentials*

Clear this MCP session's stored credentials. Subsequent data tools
(peers/exec/push/pull/logs) will require a fresh wanctl_login.

**On the command line.**

On the CLI this also stops a background agent started by `wanctl start`,
because a device that can no longer authenticate should not keep a dead
connection open.

```
wanctl logout
```

```
wanctl_logout{}
```

## `wanctl peers` / `wanctl_peers`

*List reachable devices and whether their identity is pinned*

List devices currently reachable by the active controller token. Returns each
stable device ID with its display label and whether this session has pinned
its identity yet ('identity: pinned' / 'identity: unpinned'); structured
content contains backward-compatible devices and aliases fields plus an
identity map keyed by the canonical namespace/device. Purely a local lookup —
it dials nothing. Use this FIRST when the user asks 'what devices are
available' or before guessing a target.

```
wanctl peers
```

```
wanctl_peers{}
```

| Error | What to do |
|---|---|
| `LOGIN REQUIRED` | No usable credential. Run `wanctl login` (CLI) or wanctl_login (MCP); an MCP session that had one can restore it instantly with wanctl_login(rebind=…). |

## `wanctl pair` / `wanctl_pair`

*Check a device's trust state, or get the URL to pair with it*

Check whether the target device already trusts this MCP session's controller
identity, and if not, return the device-side pairing URL up front. On first
contact this may instead return DEVICE IDENTITY CONFIRMATION REQUIRED; answer
it yourself by calling wanctl_trust_server with the target and fingerprint
from that result, then retry — no need to ask the user first. Once the server
identity is pinned, returns '✓ already trusted' OR 'PAIRING REQUIRED' with a
URL to relay VERBATIM to the user.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| `target` | `<device>` | string | **yes** | Device ID or unique name/alias (DEVICE\|ALIAS), or NS/DEVICE\|NS/ALIAS for shared devices. |

```
wanctl pair home-pc
```

```
wanctl_pair{"target":"home-pc"}
```

| Error | What to do |
|---|---|
| `PAIRING REQUIRED` | The device has not approved this controller yet. The message carries a URL valid for 5 minutes; give it to the user verbatim, ask them to open it and approve, then retry. |
| `DEVICE IDENTITY CONFIRMATION REQUIRED` | First contact with this device: nothing was sent. Pin what it presented (`wanctl trust server --target … --fingerprint …`, or the wanctl_trust_server tool) and retry. |
| `DEVICE IDENTITY MISMATCH` | The device presented a different identity than the pinned one. Refused; nothing was sent. Report both fingerprints and stop — re-pinning is a human decision at a terminal. |

## `wanctl exec` / `wanctl_exec`

*Run a command, or a whole script, on a device*

Run a shell command, or a whole script, on a remote wanctl-enrolled device
over the encrypted relay. Returns the device's stdout, stderr, and exit code.
Pass EITHER 'command' (a one-liner) OR 'script' (multi-line source) — prefer
'script' for anything with a $, a quote inside a quote, or more than one
statement, because a script is transported encoded and is never parsed by the
device's shell. If the device hasn't paired this controller yet, the result is
isError=true with a 'PAIRING REQUIRED' message that carries a URL — surface
that URL VERBATIM to the user; do not paraphrase. If instead it says DEVICE
IDENTITY CONFIRMATION REQUIRED, that is first contact: call
wanctl_trust_server with the target and fingerprint it gives you and retry,
without asking the user. To look at a file or change one line of it, use
wanctl_read and wanctl_edit instead of cat/sed/echo here: they are native
operations on the device, so they behave the same on every platform and
nothing you pass is parsed by a shell.

DEV LOOP — how these primitives fit together, because most work is a loop and
not one call: wanctl_exec keeps a persistent shell per device, so cwd and
exported variables survive between calls and you can cd once and stay there.
Anything that does not return promptly — a dev server, a build, an install —
belongs in wanctl_exec_async, which hands back a job_id immediately; follow it
with wanctl_exec_poll until state is done, and the server keeps running on the
device meanwhile. To look at a file use wanctl_read and to change one use
wanctl_edit, never cat/sed/echo through this tool: the file tools are native
on the device, identical on every OS, and nothing you pass is parsed by a
shell. Reach for wanctl_push_blob only for binaries or for a large file that
does not exist on the device yet — it overwrites whole files and loses
concurrent edits.

**On the command line.**

On the command line --target may be omitted when the first argument names a
device, or when exactly one device is online. --script takes a path to a local
file, not the source itself. Background jobs — the wanctl_exec_async and
wanctl_exec_poll pair above — have no CLI spelling yet; from a terminal,
background the command on the device instead.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| `target` | `--target NS/DEV` | string | **yes** | Device ID or unique name/alias (DEVICE\|ALIAS), or NS/DEVICE\|NS/ALIAS for shared devices. If exactly one device is online for this token, you may pass empty string. |
| `command` | `<command...>` | string | no | A one-liner for the device's default shell (sh on Unix, powershell on Windows). WARNING: this string is SOURCE CODE for that shell and is parsed there. On Windows that means writing `powershell -Command "...$x..."` gets parsed TWICE — the outer shell expands $x to nothing and the inner script fails with a misleading 'term is not recognized'. Use 'script' instead of nesting an interpreter here. **On the CLI:** The command to run, given as the trailing arguments. It is SOURCE CODE for the device's shell (sh on Unix, powershell on Windows) and is parsed there, so a nested `powershell -Command "...$x..."` is parsed twice and loses its variables to the outer pass; wanctl warns when it sees that shape. Use --script instead of nesting an interpreter. |
| `script` | `--script <local-file>` | string | no | Script SOURCE to run on the device (not a file path). Sent encoded, so quoting and character-set rules do not apply: $, backticks, nested quotes and non-ASCII text all arrive literally. Requires 'interp'. Use this for multi-statement work; it is the same single call as 'command'. Scripts over ~9KB must be pushed as a file and run by path instead. **On the CLI:** Path to a script FILE on this machine. Its contents are sent base64-encoded and run on the device, so quoting and character-set rules do not apply: $, backticks, nested quotes and non-ASCII text all arrive literally. The interpreter comes from the extension (.ps1 → PowerShell, .sh or none → sh) unless --interp says otherwise. This is the CLI spelling of the MCP `script` argument, which carries the source itself rather than a path. |
| `interp` | `--interp powershell\|sh` | string | no | Interpreter for 'script': 'powershell' for Windows devices, 'sh' for Unix/macOS/Android. Required when 'script' is set. **On the CLI:** Override the interpreter --script would infer from the file extension: powershell \| sh. |
| `cwd` | — | string | no | Working directory on the device for this command (also the policy scope). |
| `oneshot` | `--oneshot` | boolean | no | Run in a fresh shell with no persistent session state. Default false — successive exec calls share cwd/env like a real terminal. |
| `elevate` | `--elevate` | boolean | no | Android only. Run with elevated privilege (uid 0 or the adb shell uid 2000) instead of the app sandbox the agent normally lives in. This is what makes `pm`, `am`, `input`, `screencap`, `dumpsys`, `settings`, `wm` and `svc` work at all — without it they fail with permission errors or empty output. Elevated commands need their OWN policy rule on the device; a device in bypass mode still refuses them until a human approves, so expect a 'PAIRING/approval' style rejection the first time. |
| `via` | `--via su\|adb` | string | no | Pin the elevation channel: 'su' (rooted device) or 'adb' (device's own wireless debugging). Default empty = let the device pick whichever is available. Naming an unavailable channel fails instead of quietly running unprivileged. |

```
wanctl exec --target home-pc "uname -a"
  wanctl exec --target home-pc --script ./setup.sh
```

```
wanctl_exec{"target":"home-pc","command":"uname -a"}
```

| Error | What to do |
|---|---|
| `PAIRING REQUIRED` | The device has not approved this controller yet. The message carries a URL valid for 5 minutes; give it to the user verbatim, ask them to open it and approve, then retry. |
| `DEVICE IDENTITY CONFIRMATION REQUIRED` | First contact with this device: nothing was sent. Pin what it presented (`wanctl trust server --target … --fingerprint …`, or the wanctl_trust_server tool) and retry. |
| `DEVICE IDENTITY MISMATCH` | The device presented a different identity than the pinned one. Refused; nothing was sent. Report both fingerprints and stop — re-pinning is a human decision at a terminal. |
| `LOGIN REQUIRED` | No usable credential. Run `wanctl login` (CLI) or wanctl_login (MCP); an MCP session that had one can restore it instantly with wanctl_login(rebind=…). |

## `wanctl read` / `wanctl_read`

*Read a range of lines from a text file on a device*

Read a range of lines from a text file on a remote wanctl-enrolled device. Use
this instead of `wanctl_exec` with cat/head/sed/Get-Content whenever you want
to LOOK at a file: it is performed natively on the device, so it behaves
identically on Linux, macOS, Windows and Android, nothing is parsed by a
shell, and the reply tells you exactly what you got — 'lines A-B of N', the
file's size, and the sha256 OF THE WHOLE FILE. Save that sha256: passing it
back as wanctl_edit's expected_sha256 is how you make sure you are patching
the text you actually read. Reads at most 256 KiB of content per call, and
always cuts on a line boundary: when the range is cut short the result says
truncated=true and names the `offset` to continue from, so paging never loses
or repeats a line. The one exception is a single line bigger than 256 KiB,
which cannot be returned whole — the result names that line and tells you to
read it with wanctl_exec (sed/cut) instead; do not page on, because the same
line would come back every time. Errors: 'not a UTF-8 text file' means the
file is binary — use wanctl_pull or wanctl_exec instead, do not retry. Same
pairing/policy rules as wanctl_exec: 'PAIRING REQUIRED' carries a URL to relay
VERBATIM to the user, 'DEVICE IDENTITY CONFIRMATION REQUIRED' means call
wanctl_trust_server and retry, and 'read denied by device policy' means the
device's owner has not granted read access to that path.

**On the command line.**

The content goes to stdout and the trailer — line numbers, the whole file's
sha256, whether the 256 KiB cap cut the range short — goes to stderr, so
`wanctl read … > file` captures exactly the file content.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| `target` | `--target NS/DEV` | string | **yes** | Device ID or unique name/alias (DEVICE\|ALIAS), or NS/DEVICE\|NS/ALIAS for shared devices. |
| `path` | `<path>` | string | **yes** | Absolute path on the target device. `~` is NOT expanded — spell the home directory out. |
| `offset` | `--offset N` | number | no | 1-based line number to start at. Default 1. Use this to page through a file that came back truncated. |
| `limit` | `--limit N` | number | no | Maximum number of lines to return. Default 2000. The 256 KiB byte cap applies regardless. |

```
wanctl read --target home-pc /etc/hosts --offset 1 --limit 200
```

```
wanctl_read{"target":"home-pc","path":"/etc/hosts","limit":200}
```

| Error | What to do |
|---|---|
| `not a UTF-8 text file` | The file is binary. Use pull (or exec) instead; retrying read will not help. |
| `read denied by device policy` | The device owner has not granted read access to that path. |
| `PAIRING REQUIRED` | The device has not approved this controller yet. The message carries a URL valid for 5 minutes; give it to the user verbatim, ask them to open it and approve, then retry. |
| `DEVICE IDENTITY CONFIRMATION REQUIRED` | First contact with this device: nothing was sent. Pin what it presented (`wanctl trust server --target … --fingerprint …`, or the wanctl_trust_server tool) and retry. |
| `does not support read/edit; run `wanctl update`` | The device is running a wanctl older than the file tools. Update it there, then retry. |

## `wanctl edit` / `wanctl_edit`

*Replace a string inside a file on a device, atomically*

Replace a string inside a text file on a remote device, in place. This is the
tool for PATCHING a remote file: it is the exact-string edit you are used to
locally, performed natively on the device, so no shell parses your text ($,
backticks, quotes and newlines all arrive literally) and the rest of the file
is preserved byte for byte — CRLF line endings stay CRLF, the file mode is
kept, and the write is atomic (temp file + rename), so a reader never sees a
half-written file. Prefer it over rewriting a whole file with
wanctl_push_blob, which silently discards anything that changed since you last
read the file.

WORKFLOW: wanctl_read the file, copy its sha256 into expected_sha256 here, and
pass enough surrounding text in `old` that it matches exactly once.

REFUSALS (the file is left untouched every time — fix the input and retry, do
not fall back to exec): 'old string not found' means your `old` does not
appear, usually because of whitespace or indentation, so re-read the file
rather than guessing; 'old string occurs N times' means you must add
surrounding context to disambiguate, or pass all=true if you really do mean
every occurrence; 'changed since it was read' means someone else wrote to the
file — the message carries the file's CURRENT sha256, so re-read and redo the
edit against the new text. Files over 8 MiB are refused. Policy: an edit is a
WRITE on the device and needs the same grant as wanctl_push; a first edit on
an unapproved path may wait for the device owner to approve it.

**On the command line.**

--old-file and --new-file read the text from a local file, which is how a
multi-line block gets through without fighting the shell over quoting. Giving
both --old and --old-file is an error rather than a precedence rule.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| `target` | `--target NS/DEV` | string | **yes** | Device ID or unique name/alias (DEVICE\|ALIAS), or NS/DEVICE\|NS/ALIAS for shared devices. |
| `path` | `<path>` | string | **yes** | Absolute path on the target device. `~` is NOT expanded. |
| `old` | `--old STR \| --old-file F` | string | **yes** | The exact text to find, copied from a wanctl_read of this file. Must be non-empty, and must match exactly once unless `all` is true. |
| `new` | `--new STR \| --new-file F` | string | **yes** | The text to put in its place. May be an empty string, which deletes `old`. |
| — | `--old-file F` | string | no | Read the text to find from this local file instead of --old. This is how a multi-line block gets through without fighting the shell over quoting. Giving both --old and --old-file is an error, not a precedence rule. |
| — | `--new-file F` | string | no | Read the replacement from this local file instead of --new. |
| `all` | `--all` | boolean | no | Replace every occurrence instead of refusing when `old` appears more than once. Default false. |
| `expected_sha256` | `--sha SHA256` | string | no | The sha256 wanctl_read reported for this file. When set, the edit is refused if the file no longer hashes to it, so a concurrent change cannot be overwritten silently. Strongly recommended. |

```
wanctl edit --target lab /app.conf --old "port = 80" --new "port = 8080"
  wanctl edit --target lab /app.conf --old X --new Y --sha <from read>
```

```
wanctl_edit{"target":"lab","path":"/app.conf","old":"80","new":"8080"}
```

| Error | What to do |
|---|---|
| `old string not found` | The `old` text does not appear, usually a whitespace or indentation difference. Re-read the file instead of guessing. |
| `old string occurs N times` | Add surrounding context so it matches once, or pass --all / all=true if every occurrence is meant. |
| `changed since it was read` | Someone else wrote to the file. The message carries the current sha256; re-read and redo the edit. |
| `PAIRING REQUIRED` | The device has not approved this controller yet. The message carries a URL valid for 5 minutes; give it to the user verbatim, ask them to open it and approve, then retry. |
| `DEVICE IDENTITY CONFIRMATION REQUIRED` | First contact with this device: nothing was sent. Pin what it presented (`wanctl trust server --target … --fingerprint …`, or the wanctl_trust_server tool) and retry. |
| `does not support read/edit; run `wanctl update`` | The device is running a wanctl older than the file tools. Update it there, then retry. |

## `wanctl_exec_async`

*Start a background job and return its id at once*

Start a shell command as a BACKGROUND job on the device and return a job_id
IMMEDIATELY, without waiting for it to finish. Use this for anything that may
run longer than a single tool call comfortably tolerates — package installs,
builds, large downloads, `wsl --shutdown` then a long build, etc. The command
keeps running on the device even after this call returns; fetch its output and
exit code later with wanctl_exec_poll(job_id). Always runs in a FRESH shell
(no shared cwd/env with wanctl_exec's persistent session). Same pairing/policy
rules as wanctl_exec. Jobs run for at most 30 minutes, retain at most 8 MiB
output each, and finished results remain pollable for up to 1h subject to
device-wide retention budgets.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| `target` | — | string | **yes** | Device ID or unique name/alias (DEVICE\|ALIAS), or NS/DEVICE\|NS/ALIAS. |
| `command` | — | string | **yes** | Shell command to run in the device's default shell (sh on Unix, powershell on Windows). |
| `cwd` | — | string | no | Working directory on the device for this command (also the policy scope). |

```
wanctl_exec_async{"target":"lab","command":"npm run dev","cwd":"/srv"}
```

| Error | What to do |
|---|---|
| `PAIRING REQUIRED` | The device has not approved this controller yet. The message carries a URL valid for 5 minutes; give it to the user verbatim, ask them to open it and approve, then retry. |
| `DEVICE IDENTITY CONFIRMATION REQUIRED` | First contact with this device: nothing was sent. Pin what it presented (`wanctl trust server --target … --fingerprint …`, or the wanctl_trust_server tool) and retry. |
| `LOGIN REQUIRED` | No usable credential. Run `wanctl login` (CLI) or wanctl_login (MCP); an MCP session that had one can restore it instantly with wanctl_login(rebind=…). |

## `wanctl_exec_poll`

*Fetch a background job's new output and status*

Fetch a background job's new output and status (started via
wanctl_exec_async). Call repeatedly until state is 'done'. Pass the
'next_offset' from the previous poll as 'offset' to receive only NEW output
each time; omit or 0 to get everything from the start. The response carries a
status header (state: running|done, exit code when done, next_offset) followed
by the output.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| `target` | — | string | **yes** | Device ID or unique name/alias (DEVICE\|ALIAS), or NS/DEVICE\|NS/ALIAS — the same device the job was started on. |
| `job_id` | — | string | **yes** | The job id returned by wanctl_exec_async. |
| `offset` | — | number | no | Bytes of output already seen; return only output past this point. Use the previous poll's next_offset. Default 0 = from the start. |

```
wanctl_exec_poll{"target":"home-pc","job_id":"j-7f2","offset":4096}
```

## `wanctl push` / `wanctl_push`

*Upload a local file to a path on the device*

Upload a local file to a remote path on the target device. Same pairing/policy
rules as wanctl_exec. Available in stdio mode only (on a shared HTTP MCP
server 'local' would be a path on the server itself). Paths under a
dot-directory of the operator's home (~/.ssh, ~/.config, …) are refused;
WANCTL_MCP_LOCAL_ROOT confines the tool to one tree. To change part of a file
that is already on the device, use wanctl_edit rather than uploading a
rewritten copy.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| `target` | `--target NS/DEV` | string | **yes** | Device ID or unique name/alias (DEVICE\|ALIAS), or NS/DEVICE\|NS/ALIAS. |
| `local` | `<local>` | string | **yes** | Absolute path on the MCP-server machine (or your local machine in stdio mode) to upload. |
| `remote` | `<remote>` | string | **yes** | Absolute path on the target device to write to. |

```
wanctl push --target home-pc ./build/app /opt/app/app
```

```
wanctl_push{"target":"lab","local":"/tmp/app","remote":"/opt/app"}
```

| Error | What to do |
|---|---|
| `PAIRING REQUIRED` | The device has not approved this controller yet. The message carries a URL valid for 5 minutes; give it to the user verbatim, ask them to open it and approve, then retry. |
| `DEVICE IDENTITY CONFIRMATION REQUIRED` | First contact with this device: nothing was sent. Pin what it presented (`wanctl trust server --target … --fingerprint …`, or the wanctl_trust_server tool) and retry. |
| `LOGIN REQUIRED` | No usable credential. Run `wanctl login` (CLI) or wanctl_login (MCP); an MCP session that had one can restore it instantly with wanctl_login(rebind=…). |

## `wanctl_push_blob`

*Upload inline base64 content to a path on the device*

Upload INLINE base64 content to a remote path on the target device — the
file-push tool that works in HTTP (remote) MCP mode, where the AI host has no
file on the MCP server for wanctl_push to read. Encode the bytes you want
written as base64 and pass them in 'content_b64'. Same pairing/policy rules as
wanctl_exec. Size cap: 8 MiB of raw (decoded) bytes; for larger payloads,
split or have the device fetch the file itself. This tool OVERWRITES the whole
file, discarding anything changed since you last read it — to patch an
existing file, use wanctl_edit instead.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| `target` | — | string | **yes** | Device ID or unique name/alias (DEVICE\|ALIAS), or NS/DEVICE\|NS/ALIAS. |
| `remote` | — | string | **yes** | Absolute path on the target device to write to (overwrites if it exists). |
| `content_b64` | — | string | **yes** | Standard-base64-encoded file content (the RAW bytes to write, not text). |
| `mode` | — | string | no | Optional octal file mode, e.g. "0755" for an executable. Default 0644. |

```
wanctl_push_blob{"target":"lab","remote":"/opt/run.sh",
                   "content_b64":"…","mode":"0755"}
```

| Error | What to do |
|---|---|
| `PAIRING REQUIRED` | The device has not approved this controller yet. The message carries a URL valid for 5 minutes; give it to the user verbatim, ask them to open it and approve, then retry. |
| `DEVICE IDENTITY CONFIRMATION REQUIRED` | First contact with this device: nothing was sent. Pin what it presented (`wanctl trust server --target … --fingerprint …`, or the wanctl_trust_server tool) and retry. |

## `wanctl pull` / `wanctl_pull`

*Download a file from the device to a local path*

Download a remote file from the target device to a local path. Same
pairing/policy rules as wanctl_exec. Available in stdio mode only; the same
local-path limits as wanctl_push apply. To inspect a text file rather than
keep a copy of it — including on a shared HTTP MCP server, where this tool is
unavailable — use wanctl_read.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| `target` | `--target NS/DEV` | string | **yes** | Device ID or unique name/alias (DEVICE\|ALIAS), or NS/DEVICE\|NS/ALIAS. |
| `remote` | `<remote>` | string | **yes** | Absolute path on the target device to read. |
| `local` | `<local>` | string | **yes** | Absolute path on the MCP-server machine (or your local machine in stdio mode) to write to. |

```
wanctl pull --target home-pc /var/log/app.log ./app.log
```

```
wanctl_pull{"target":"lab","remote":"/var/log/app.log","local":"./app"}
```

| Error | What to do |
|---|---|
| `PAIRING REQUIRED` | The device has not approved this controller yet. The message carries a URL valid for 5 minutes; give it to the user verbatim, ask them to open it and approve, then retry. |
| `DEVICE IDENTITY CONFIRMATION REQUIRED` | First contact with this device: nothing was sent. Pin what it presented (`wanctl trust server --target … --fingerprint …`, or the wanctl_trust_server tool) and retry. |
| `LOGIN REQUIRED` | No usable credential. Run `wanctl login` (CLI) or wanctl_login (MCP); an MCP session that had one can restore it instantly with wanctl_login(rebind=…). |

## `wanctl logs` / `wanctl_logs`

*Read a device's activity log: connects, execs, file operations*

Pull JSONL activity events from the target device's local log (every
connect/exec/file with its decision and exit code). Useful for auditing what
happened, including past pairing/approval outcomes.

**On the command line.**

With no --target this reads THIS machine's own log, which is how a device
owner sees what controllers did to it. `wanctl logs --service portal|relay`
reads server logs instead; --follow is not supported.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| `target` | `--target NS/DEV` | string | **yes** | Device ID or unique name/alias (DEVICE\|ALIAS), or NS/DEVICE\|NS/ALIAS. |
| `type` | `--type T` | string | no | Filter: 'connect', 'exec', or 'file'. |
| `grep` | `--grep STR` | string | no | Filter: substring of the detail field. |
| `since` | `--since RFC3339` | string | no | Filter: RFC3339 timestamp lower bound. |
| `limit` | `--limit N` | number | no | Return at most this many of the most recent matching events (0 = no cap). |
| — | `--service portal\|relay` | string | no | Read a SERVER's process log instead of a device's activity log. Needs WANCTL_ADMIN_SECRET; the MCP spelling is the separate tool wanctl_server_logs. |
| — | `--follow` | boolean | no | Not yet supported. Named here so that asking for it fails loudly instead of silently printing a snapshot. |

```
wanctl logs --target lab --type exec --since 2026-09-17T00:00:00Z
```

```
wanctl_logs{"target":"home-pc","type":"exec","limit":50}
```

| Error | What to do |
|---|---|
| `PAIRING REQUIRED` | The device has not approved this controller yet. The message carries a URL valid for 5 minutes; give it to the user verbatim, ask them to open it and approve, then retry. |
| `DEVICE IDENTITY CONFIRMATION REQUIRED` | First contact with this device: nothing was sent. Pin what it presented (`wanctl trust server --target … --fingerprint …`, or the wanctl_trust_server tool) and retry. |

## `wanctl logs --service` / `wanctl_server_logs`

*Read recent portal or relay process logs*

Read recent portal or relay process logs through the secret-gated admin API.
Output is redacted before filtering and bounded by the requested limit.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| `service` | `--service portal\|relay` | string | **yes** | Server service: 'portal' or 'relay'. |
| `since` | `--since 15m` | string | no | Lookback duration such as '30m' or '2h'. Default 15m. |
| `limit` | `--limit N` | number | no | Return at most this many recent lines. Default 200, maximum 2000. |
| `grep` | `--grep STR` | string | no | Filter by substring after credential redaction. |

```
wanctl logs --service relay --since 30m --grep pairing
```

```
wanctl_server_logs{"service":"relay","since":"30m"}
```

| Error | What to do |
|---|---|
| `WANCTL_ADMIN_SECRET` | The admin API is secret-gated; without the secret in the environment the call is refused. |

## `wanctl id` / `wanctl_id`

*Show this controller identity's fingerprint*

Show THIS MCP session's controller identity fingerprint. The fingerprint is
what target devices pair against in the trust step.

```
wanctl id
```

```
wanctl_id{}
```

## `wanctl trust` / `wanctl_trust`

*List the trust store*

List the trust store for THIS MCP session. 'servers' (default) = explicitly
pinned devices. 'clients' = controllers this machine has trusted to drive it
(only meaningful in stdio mode if this machine is also running wanctl agent).

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| `which` | `[clients\|servers]` | string | no | 'servers' (default) or 'clients'. |

```
wanctl trust servers
```

```
wanctl_trust{"which":"servers"}
```

## `wanctl trust server` / `wanctl_trust_server`

*Pin a device's identity for this controller*

Pin a device's identity for THIS session. Call it as soon as any tool returns
DEVICE IDENTITY CONFIRMATION REQUIRED, passing the target and fingerprint
copied VERBATIM from that result, then retry the call that failed — do not ask
the user to confirm the fingerprint first, because your MCP client's own
approval prompt is already the human checkpoint. This is a first-contact step
only: it records what the device presented right now, so later calls can
detect a change. If a call instead returns 'DEVICE IDENTITY MISMATCH', do NOT
call this tool — report both the pinned and the presented fingerprint to the
user and stop; re-pinning is a human's decision at a terminal. Note: the
handler refuses unless the operator has set
WANCTL_MCP_ALLOW_UNSAFE_TRUST_SERVER=1 (the hosted endpoint does; a local
stdio server usually does not, and there a human runs `wanctl trust server`
instead).

**On the command line.**

--replace overwrites an existing pin. That is the deliberate human step after
a DEVICE IDENTITY MISMATCH has been investigated and explained; without it,
re-pinning a known device is refused.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| `target` | `--target NS/DEV` | string | **yes** | The owner/device target, copied verbatim from the DEVICE IDENTITY CONFIRMATION REQUIRED result. |
| `fingerprint` | `--fingerprint SHA256:...` | string | **yes** | The SHA256:... fingerprint, copied verbatim from the same result. |
| — | `--replace` | boolean | no | Overwrite an existing pin. Without it, re-pinning a known device is refused — that refusal is the whole point of a pin, so passing this is a deliberate human act after a mismatch has been explained. |

```
wanctl trust server --target ns/home-pc --fingerprint SHA256:… [--replace]
```

```
wanctl_trust_server{"target":"ns/home-pc","fingerprint":"SHA256:…"}
```

| Error | What to do |
|---|---|
| `DEVICE IDENTITY MISMATCH` | The device presented a different identity than the pinned one. Refused; nothing was sent. Report both fingerprints and stop — re-pinning is a human decision at a terminal. |

## `wanctl rules` / `wanctl_rules`

*List the policy rules this machine enforces on controllers*

List the local policy rules (allow-list) on THIS machine. Only meaningful in
stdio mode if this machine is also running wanctl agent; for controller-only
and HTTP-mode hosts the list is empty.

**On the command line.**

The CLI also writes: `wanctl rules add` appends a rule and `wanctl rules rm`
removes one. Rules are enforced by the agent on THIS machine, so they only
mean anything where a device runs.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| — | `--kind exec\|read\|write\|logs` | string | no | What the rule governs. `wanctl rules add` only. |
| — | `--pattern P` | string | no | For exec, a command prefix with an optional trailing *. For file kinds, a directory. `wanctl rules add` only. |
| — | `--dir D` | string | no | For an exec rule scoped to a working directory. `wanctl rules add` only. |

```
wanctl rules
  wanctl rules add --kind exec --pattern "git *"
```

```
wanctl_rules{}
```

## `wanctl start`

*Turn this machine into a controlled device*

Log in if there is no credential yet, then run the agent detached in the
background. This is the command that makes a machine a controlled device:
until it runs, nothing can dial in.

Persistence: `wanctl start` survives THIS terminal but may not survive logout
or reboot. `wanctl service install` adds OS-native autostart; Linux
additionally needs user lingering to come up without a login, and Windows
starts the limited-user task at the next logon.

```
wanctl start
```

| Error | What to do |
|---|---|
| `LOGIN REQUIRED` | No usable credential. Run `wanctl login` (CLI) or wanctl_login (MCP); an MCP session that had one can restore it instantly with wanctl_login(rebind=…). |

## `wanctl stop`

*Stop the agent started by `wanctl start`*

Signal the background agent to shut down and wait for it to release the
config-dir lock. An agent that is mid-command finishes it first.

```
wanctl stop
```

## `wanctl service`

*Install, remove or inspect an OS-native always-on service*

Install the agent as a systemd user unit, a launchd agent or a Windows
Scheduled Task, so it comes back without anyone logging in and typing `wanctl
start`.

--name and --portal-fps are baked into the unit, because a unit restarts
unattended and cannot be asked. Omit --mode so the persisted mode and portal
switches survive a restart instead of being frozen at install time.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| — | `--name N` | string | no | Device name baked into the unit. |
| — | `--portal-fps FP[,FP]` | string | no | Portal fingerprints the agent will accept, baked into the unit. |
| — | `--mode M` | string | no | Freeze the policy mode in the unit. Omit it so the persisted mode survives a restart. |
| — | `--relay URL` | string | no | Relay URL baked into the unit. Defaults to the currently configured relay. |
| — | `--transport ws\|http` | string | no | Transport baked into the unit. Defaults to the currently configured transport. |

```
wanctl service install --name lab-box
  wanctl service status
  wanctl service uninstall
```

## `wanctl agent`

*Run the agent in the foreground*

Run the device-side agent attached to this terminal — what `wanctl start` and
the OS service spawn. Use it to watch policy decisions land in real time, or
under your own supervisor.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| — | `--name N` | string | no | Display name for this device. Defaults to the hostname; it does not change the device ID. |
| — | `--relay URL` | string | no | Relay URL. Defaults to the configured one. |
| — | `--token T` | string | no | Access or registration token. Defaults to WANCTL_TOKEN, then the stored token. |
| — | `--transport ws\|http` | string | no | Transport to the relay. http is proxy-agnostic and works through ordinary reverse proxies. |
| — | `--mode normal\|bypass` | string | no | Policy mode. normal prompts on a rule miss; bypass auto-allows and is DANGEROUS. Empty keeps the last persisted mode. |
| — | `--shell S` | string | no | Shell for exec. Defaults to powershell on Windows and /bin/sh elsewhere. |
| — | `--yes` | boolean | no | Auto-trust new controllers. For unattended devices only: it removes the human approval step. |
| — | `--portal-fps FP[,FP]` | string | no | Comma-separated portal admin fingerprints to seed locally, so this device accepts that portal's approvals. |
| — | `--managed` | boolean | no | The agent is owned by an external supervisor, so it does not try to restart itself. |

```
wanctl agent --name lab-box --relay https://relay.example.com
```

## `wanctl update`

*Replace this binary with the latest signed release*

Fetch the release manifest, verify the signature and swap this binary in
place. A running agent keeps its pid across the swap, so systemd, launchd and
`wanctl status` still point at it. Development builds are never replaced.

On Android the APK cannot be installed by the binary, so --fetch-apk downloads
and verifies it and prints the path for the app to install.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| — | `--fetch-apk DIR` | string | no | Android only. Download and verify the APK into this directory, print its path and exit. The binary cannot install an APK; the app does that with the printed path. |
| — | `--no-restart` | boolean | no | Internal. Skips the daemon stop/start, used by the sudo-elevated second phase of an update. |

```
wanctl update
  wanctl update --fetch-apk /sdcard/Download
```

## `wanctl version`

*Print the release version*

Print the immutable release version baked into this binary, or `dev` for a
local build.

```
wanctl version
```

## `wanctl mcp`

*Run wanctl as an MCP server*

Serve the tools in this contract over the Model Context Protocol.

With no flags it speaks stdio: one process per AI host, single user, backed by
this machine's wanctl config. With --http it serves Streamable HTTP for many
users at once, deriving a separate controller identity per namespace from
WANCTL_MCP_SEED; wanctl_push and wanctl_pull are withdrawn there, because
`local` would name a path on the server rather than on the caller's machine.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| — | `--http ADDR` | string | no | Serve Streamable HTTP on this address (e.g. :8081) instead of stdio. Multi-user: each session derives its own controller identity from WANCTL_MCP_SEED, and wanctl_push/wanctl_pull are withdrawn because 'local' would name a path on the server. |

```
wanctl mcp
  wanctl mcp --http :8081
```

## `wanctl docs`

*Read and write the portal's documentation articles*

List, read, create, edit and remove the articles the portal serves, and the
groups they sit in. Bodies come from --file, from $EDITOR with --editor, or
from stdin.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| — | `--slug S` | string | no | URL slug, unique across articles. |
| — | `--title T` | string | no | Human title. |
| — | `--group G` | string | no | Group slug: a filter for `docs ls`, the destination for `docs new` and `docs edit`. |
| — | `--position N` | string | no | Sort order within the group. |
| — | `--file F` | string | no | Read the body from this local file. Without it, and without --editor, the body is read from stdin. |
| — | `--editor` | boolean | no | Open $EDITOR to write the body. |

```
wanctl docs ls --group quickstart
  wanctl docs get enroll-device
  wanctl docs edit enroll-device --file ./enroll.md
```

## `wanctl friends`

*List and manage friend relationships between namespaces*

Sharing a device is only possible between namespaces that have agreed to it,
so a friendship is the prerequisite for `wanctl share`. This lists
relationships and pending requests, and sends, accepts, declines or removes
them.

```
wanctl friends
  wanctl friends add other-ns
  wanctl friends accept other-ns
```

## `wanctl share`

*Grant another namespace the use of one of your devices*

Sharing hands a friend the use of a device, not ownership of it: they drive it
under your device's policy, and --manage additionally lets them change that
policy. Revoking takes effect on their next call.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| — | `--device DEV` | string | no | The device of yours being shared. |
| — | `--to NS` | string | no | The friend namespace receiving it. They must already be a friend. |
| — | `--manage` | boolean | no | Also let them administer the device — approvals, rules and mode — not just use it. |

```
wanctl share grant --device home-pc --to other-ns
  wanctl share manage --device home-pc --to other-ns on
  wanctl share revoke --device home-pc --to other-ns
```

## `wanctl screenshot`

*Capture an Android device's screen to a local PNG*

`screencap -p` writes a PNG to stdout, and through a shell pipeline stdout is
a terminal — a screenful of binary. This writes a file instead, and only
writes to stdout when explicitly asked with `-o -`. Implies --elevate, because
screencap needs it.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| — | `[DEVICE] \| --target NS/DEV` | string | no | Device ID or unique name. It may also be the first positional argument. |
| — | `-o FILE` | string | no | Local file to write. Defaults to screenshot-<device>-<time>.png; "-" writes the PNG to stdout instead, which is the only way to pipe it. |
| — | `--via su\|adb` | string | no | Pin the elevation channel: 'su' (rooted device) or 'adb' (the device's own wireless debugging). Default empty lets the device pick. |

```
wanctl screenshot home-phone -o ./screen.png
```

| Error | What to do |
|---|---|
| `PAIRING REQUIRED` | The device has not approved this controller yet. The message carries a URL valid for 5 minutes; give it to the user verbatim, ask them to open it and approve, then retry. |
| `DEVICE IDENTITY CONFIRMATION REQUIRED` | First contact with this device: nothing was sent. Pin what it presented (`wanctl trust server --target … --fingerprint …`, or the wanctl_trust_server tool) and retry. |

## `wanctl config`

*Show or persist relay, portal and transport settings*

Show the effective settings and where each came from — a flag, an environment
variable, the config file or a value baked into the build — then persist or
remove the file-backed ones.

```
wanctl config
  wanctl config set relay=https://relay.example.com
  wanctl config unset portal
```

## `wanctl label`

*Show or set this controller's self-description*

A device refuses to raise a pairing request from a controller that has not
said who it is, because the approval screen would otherwise ask a human to
trust an anonymous fingerprint.

```
wanctl label "Lin's laptop, Claude Code"
```

## `wanctl admin`

*Mint, list and revoke admission invites*

Instance administration for an invite-only relay. Needs WANCTL_ADMIN_SECRET;
--github pre-approves a GitHub login instead of minting a code to hand out.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| — | `--github LOGIN` | string | no | Pre-approve this GitHub login instead of minting a code to hand out. |

```
wanctl admin invite --github octocat
  wanctl admin invites
  wanctl admin invite-revoke INVITE-ID
```

## `wanctl portal-admins`

*Manage the local portal root fingerprints*

The fingerprints this machine accepts as the portal's own identity. Adding one
is what lets a self-hosted portal approve pairings for this device.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| — | `--fingerprints FP[,FP]` | string | no | Comma-separated SHA256 fingerprints to add or remove. |

```
wanctl portal-admins list
  wanctl portal-admins add SHA256:…
```

## `wanctl relay`

*Run the relay (the public broker)*

Run the server side that brokers byte pipes between controllers and devices.
It authenticates tokens and authorizes connections but cannot decrypt a
session: controllers and devices speak mutual TLS through the pipe, so the
relay sees ciphertext. Needs DATABASE_URL or WANCTL_TOKENS.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| — | `--addr :PORT` | string | no | Listen address. Default :8080. |

```
wanctl relay --addr :8080
```

## `wanctl portal`

*Run the web portal*

Run the web front end for login, enrollment, approvals and documentation. It
has no database of its own; it authenticates users and scopes calls to the
relay's admin API. Logs in with GitHub OAuth, or behind a trusted SSO reverse
proxy.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| — | `--addr :PORT` | string | no | Listen address. Default :8080. |

```
wanctl portal --addr :8080
```

## `wanctl help`

*Print this contract*

With no argument, print the command index. With a command name — either
spelling, `read` or `wanctl_read` — print that command's full entry: summary,
description, parameters, an example per surface and the error texts to react
to. With --markdown, print the whole catalog, which is what docs/contract.md
contains.

```
wanctl help exec
  wanctl help wanctl_read
  wanctl help --markdown > docs/contract.md
```

