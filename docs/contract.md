# wanctl command contract

wanctl is the external harness for a web AI. The AI in a chat window is the
brain; wanctl gives it hands (exec, background jobs, read, edit, push/pull),
eyes (command output, read, logs, screenshot), memory across turns (session
rebind, job ledger) and safety rails (pairing, device identity, policy rules).
Together they form one agent.

A trust layer — relay, pairing, pinned device identity, device-side policy
rules — decides who may drive which machine, and on top of it sits a
deliberately small set of primitives: run a command, run a background job,
read a file, patch a file, move bytes, read the log. There is no IDE, no
browser driver and no second way to do any of these; anything richer is built
out of them by the agent.

This file is generated. It is the output of `wanctl help --markdown`, and
the same catalog (`internal/catalog`) produces the CLI help and the MCP tool
descriptions, so the three cannot drift. Regenerate with:

```
go run . help --markdown > docs/contract.md
```

## Instructions

This is what an MCP host is handed before it calls anything — the
`instructions` field of the initialize response, and the output of
`wanctl help --instructions`. It is the harness's system prompt.

```
wanctl is the external harness for a web AI: your hands and eyes on a machine
you do not run on, behind that device owner's policy. A refusal is an answer.

  wanctl_login         log in via the portal and save the token (no daemon)
  wanctl_status        local agent and credential state, or a remote device's
  wanctl_logout        stop the agent and forget the saved login
  wanctl_peers         list the devices this token can reach
  wanctl_pair          check trust, or get the URL the device owner approves
  wanctl_exec          run a command or script on a device (persistent shell)
  wanctl_read          print a line range of a text file on a device
  wanctl_edit          replace an exact string inside a file on a device
  wanctl_write         create or replace a whole text file on a device
  wanctl_exec_async    start a background job and return its id at once
  wanctl_exec_poll     fetch a background job's new output and status
  wanctl_push          copy a local file to a device
  wanctl_push_blob     upload inline base64 content to a path on the device
  wanctl_pull          copy a file from a device to this machine
  wanctl_logs          read a device's activity log, or portal/relay server logs
  wanctl_server_logs   read recent portal or relay process logs
  wanctl_id            show this controller's identity fingerprint
  wanctl_trust         list pinned device identities, or trusted controllers
  wanctl_trust_server  pin a device's identity for this controller
  wanctl_rules         show or change this machine's local policy rules
  wanctl_screenshot    capture a device's screen as a PNG

DEV LOOP
  wanctl_exec keeps a persistent shell per device: cd once and stay there. Long
  work goes to wanctl_exec_async, then wanctl_exec_poll until it is done.
  Read with wanctl_read, patch with wanctl_edit (several {old,new} in ONE call),
  write files with wanctl_write. Never cat/sed/echo a file through a shell.
  Over-long exec output returns its TAIL; the rest waits in a device file.
  Before working in a project directory, read its AGENTS.md or CLAUDE.md with
  wanctl_read if one exists and follow it: it outranks how you would proceed.

REFUSALS — none of these mean retry as-is:
  PAIRING REQUIRED: give the URL in the message to the user, then retry.
  DEVICE IDENTITY CONFIRMATION REQUIRED: call wanctl_trust_server, retry.
  DEVICE IDENTITY MISMATCH: refused, nothing sent; report both fingerprints.
  LOGIN REQUIRED: call wanctl_login; a saved rebind restores it instantly.
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
| `write` | `wanctl_write` | Create or completely replace a text file on a device |
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
| `screenshot` | `wanctl_screenshot` | Capture a device's screen as a PNG |
| `config` | — | Show or persist relay, portal and transport settings |
| `label` | — | Show or set this controller's self-description |
| `admin` | — | Mint, list and revoke admission invites |
| `portal-admins` | — | Manage the local portal root fingerprints |
| `relay` | — | Run the relay (the public broker) |
| `portal` | — | Run the web portal |
| `help` | — | Print this contract |

## `wanctl login` / `wanctl_login`

*Authenticate to a wanctl namespace through the portal*

Get this session a credential; every other tool needs one before it can reach
a device. Reach for it when a tool comes back 'LOGIN REQUIRED', and not before
— a session that already has a credential gains nothing from logging in again.

TWO STEPS. Call with NO argument first: you get back a portal URL and a
one-time code prompt, and that URL is for the user, verbatim, because only a
human at a browser can complete it. Call again with the `code` they paste back
and it becomes a namespace token bound ONLY to this MCP session (in HTTP mode)
or to this machine's wanctl config (in stdio mode). Several AI users sharing
one MCP server each log in for themselves; no credential is ever shared
between sessions.

SAVE THE REBIND CREDENTIAL that a successful login returns. HTTP-MCP sessions
live in memory, so a relay restart or a dropped connection makes 'LOGIN
REQUIRED' surface mid-task even though the user revoked nothing and is still
authorized. That is not a reason to send them back to the portal: call
wanctl_login(rebind="…") with the credential you saved and access comes back
at once, with no round trip through a browser. Go through the OAuth flow again
only when you have no saved rebind credential.

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

Answer "am I logged in, and as what" without touching the network. Reach for
it when a tool said login was required and you are not sure whether a login
already went through, or before telling the user which namespace you are about
to act in. It reports the login state, the namespace this session is bound to,
and this controller's fingerprint.

It is not a reachability test. Whether a device exists and can be driven by
this token is wanctl_peers; whether a particular device answers right now is
the CLI's --target form. A clean status here and a failing exec are not a
contradiction.

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

Throw this session's credential away. Do it when the user asks to disconnect,
or when the session is being handed to someone else — not as housekeeping at
the end of a task, because the next tool call would then have to walk a human
back through the portal.

Afterwards every tool that touches a device — peers, exec, read, edit, write,
push, pull, logs — answers 'LOGIN REQUIRED' until wanctl_login runs again.

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

Find out what this token can actually drive, before you name a target. Reach
for it first whenever the user says a device name you have not used in this
session, or asks what is available: a guessed target costs a round trip and
returns an error that reads like a fault when it is only a typo. It is a local
lookup and dials nothing, so it stays useful even when everything else is
failing.

Each entry is a stable device ID with its display label and whether this
session has pinned that device's identity yet ('identity: pinned' / 'identity:
unpinned'). Unpinned means first contact is still ahead of you: expect DEVICE
IDENTITY CONFIRMATION REQUIRED on the first real call to it and answer that
yourself with wanctl_trust_server. Structured content carries the same devices
and aliases fields as before, plus an identity map keyed by the canonical
namespace/device.

A device the user swears exists but that is missing here is not reachable by
this token — it is offline, or in another namespace, or shared to you and
since revoked. Say that rather than retrying.

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

Ask whether a device will take orders from this session before you try to give
it one. Worth a call when you are about to start something disruptive on a
device this session has not used yet, so the user gets the approval link up
front rather than halfway through the work. Routine work does not need it:
every other tool raises the same refusals on its own.

Three answers, each with its own next move. '✓ already trusted' means go
ahead. 'PAIRING REQUIRED' carries a URL valid for five minutes — relay it to
the user VERBATIM, never paraphrased or shortened, wait for them to approve,
then retry. 'DEVICE IDENTITY CONFIRMATION REQUIRED' is first contact and is
yours to answer rather than the user's: call wanctl_trust_server with the
target and fingerprint from that result, then call this again. Do not ask
permission for that step; your MCP client's own approval prompt is the human
checkpoint.

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

Run a command on a machine you are not running on, and get back its stdout,
stderr and exit code. This is the general-purpose primitive: reach for it for
things that are genuinely commands — git, a package manager, a service check —
and not for looking at or changing files, which have their own tools and are
covered below.

Pass EITHER 'command' (a one-liner) OR 'script' (multi-line source). Choose
'script' the moment the text carries a $, a quote inside a quote, or more than
one statement: a script is transported encoded and is never parsed by the
device's shell, so what you wrote is what runs. A one-liner is SOURCE CODE for
that shell and is parsed there, which is why a nested `powershell -Command
"...$x..."` is parsed twice and fails with a misleading error.

If the device has not approved this controller yet, the result is isError=true
with a 'PAIRING REQUIRED' message carrying a URL — surface that URL to the
user VERBATIM, do not paraphrase it, and retry once they approve. 'DEVICE
IDENTITY CONFIRMATION REQUIRED' is first contact instead: call
wanctl_trust_server with the target and fingerprint it gives you and retry,
without asking the user.

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
concurrent edits. Before working inside a project directory, read its
AGENTS.md or CLAUDE.md with wanctl_read if one exists, and follow it.

LONG OUTPUT: what comes back is capped. Past the cap you get the LAST 48 KiB —
the end, where a build's error and a script's result live — behind a line that
says how many bytes there were in total and names a file ON THE DEVICE holding
the whole thing, kept for an hour. Do not re-run the command with a filter you
guessed: grep that file with another wanctl_exec. Read the line rather than
assuming the file is there: it also says when the device could not keep the
output, and when its copy holds only the first 8 MiB.

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

Before working inside a project directory, read its AGENTS.md or CLAUDE.md
with wanctl_read if one exists, and follow it: those are the project's own
instructions and they outrank how you would otherwise proceed.

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
| `result unknown: the connection dropped` | The request reached the device; the answer did not come back. A read changes nothing, so simply retry. |
| `does not support read/edit; run `wanctl update`` | The device is running a wanctl older than the file tools. Update it there, then retry. |

## `wanctl edit` / `wanctl_edit`

*Replace a string inside a file on a device, atomically*

Replace text inside a file on a remote device, in place. This is the tool for
PATCHING a remote file: it is the exact-string edit you are used to locally,
performed natively on the device, so no shell parses your text ($, backticks,
quotes and newlines all arrive literally) and the rest of the file is
preserved byte for byte — CRLF line endings stay CRLF, the file mode is kept,
and the write is atomic (temp file + rename), so a reader never sees a
half-written file. Use it for CHANGES to an existing file; to create a file or
rewrite one end to end use wanctl_write, and prefer either over
wanctl_push_blob, which silently discards anything that changed since you last
read the file.

WORKFLOW: wanctl_read the file, copy its sha256 into expected_sha256 here, and
pass enough surrounding text in `old` that it matches exactly once.

SEVERAL EDITS AT ONCE: pass `edits` — a list of {old, new} — instead of
old/new, and make ONE call with several entries rather than several calls.
Every entry matches the file as you read it, not the result of the entry
before it, so you never have to imagine the intermediate text. Keep each `old`
as SMALL as it can be while still matching exactly once: do not pad it with
unchanged lines above and below, and do not include regions you are not
changing. The whole batch is checked before anything is written — an entry
that matches twice, an entry that matches nothing, or two entries claiming the
same bytes refuses the call by index and leaves the file exactly as it was.
`all` belongs to the single old/new form only; expected_sha256 works with
both. At most 64 entries in one call — send a larger patch as several batches.

REFUSALS (the file is left untouched every time — fix the input and retry, do
not fall back to exec): 'old string not found' means your `old` does not
appear, usually because of whitespace or indentation, so re-read the file
rather than guessing; 'old string occurs N times' means you must add
surrounding context to disambiguate, or pass all=true if you really do mean
every occurrence; 'changed since it was read' means someone else wrote to the
file — the message carries the file's CURRENT sha256, so re-read and redo the
edit against the new text. Files over 8 MiB are refused. One outcome is
neither: 'result unknown: the connection dropped after the request was sent'
means the device got the request and the answer was lost, so the edit may
already be in the file — wanctl_read it and compare the sha256 before doing
anything else, and never just repeat the call. Policy: an edit is a WRITE on
the device and needs the same grant as wanctl_push; a first edit on an
unapproved path may wait for the device owner to approve it.

**On the command line.**

--old-file and --new-file read the text from a local file, which is how a
multi-line block gets through without fighting the shell over quoting. Giving
both --old and --old-file is an error rather than a precedence rule.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| `target` | `--target NS/DEV` | string | **yes** | Device ID or unique name/alias (DEVICE\|ALIAS), or NS/DEVICE\|NS/ALIAS for shared devices. |
| `path` | `<path>` | string | **yes** | Absolute path on the target device. `~` is NOT expanded. |
| `old` | `--old STR \| --old-file F` | string | no | The exact text to find, copied from a wanctl_read of this file. Must be non-empty, and must match exactly once unless `all` is true. Required unless you pass `edits` instead; giving both forms is refused rather than resolved. |
| `new` | `--new STR \| --new-file F` | string | no | The text to put in its place. May be an empty string, which deletes `old`. Belongs with `old`, not with `edits`. |
| `edits` | — | array of {old, new} | no | Several replacements applied to this file in one atomic call, as [{"old":…,"new":…}, …]. Use this instead of repeating the tool: each `old` matches the ORIGINAL text you read, each must occur exactly once, and two entries may not overlap. Keep every `old` as small as it can be while unique — padding with unchanged context is what makes an entry collide with the next one. At most 64 entries. Mutually exclusive with old/new. |
| — | `--old-file F` | string | no | Read the text to find from this local file instead of --old. This is how a multi-line block gets through without fighting the shell over quoting. Giving both --old and --old-file is an error, not a precedence rule. |
| — | `--new-file F` | string | no | Read the replacement from this local file instead of --new. |
| `all` | `--all` | boolean | no | Replace every occurrence instead of refusing when `old` appears more than once. Default false. |
| `expected_sha256` | `--sha SHA256` | string | no | The sha256 wanctl_read reported for this file. When set, the edit is refused if the file no longer hashes to it, so a concurrent change cannot be overwritten silently. Strongly recommended. |

```
wanctl edit --target lab /app.conf --old "port = 80" --new "port = 8080"
  wanctl edit --target lab /app.conf --old X --new Y --sha <from read>
```

```
wanctl_edit{"target":"lab","path":"/a.conf","old":"80","new":"8080"}
  wanctl_edit{"target":"lab","path":"/a.conf","edits":[
    {"old":"port = 80","new":"port = 8080"},
    {"old":"debug = on","new":"debug = off"}]}
```

| Error | What to do |
|---|---|
| `old string not found` | The `old` text does not appear, usually a whitespace or indentation difference. Re-read the file instead of guessing. |
| `old string occurs N times` | Add surrounding context so it matches once, or pass --all / all=true if every occurrence is meant. |
| `changed since it was read` | Someone else wrote to the file. The message carries the current sha256; re-read and redo the edit. |
| `edits[N]: old string occurs M times` | That entry of the batch is ambiguous. Nothing was written; give entry N more surrounding text and send the whole batch again. |
| `edits[N] overlaps edits[M]` | Two entries claim the same bytes. Nothing was written; merge them into one entry. |
| `over the 64-entry limit` | Too many entries in one call. Nothing was written; split the patch into several batches. |
| `pass either 'old'/'new' or 'edits', not both` | The call mixed the two forms. Pick one and resend. |
| `PAIRING REQUIRED` | The device has not approved this controller yet. The message carries a URL valid for 5 minutes; give it to the user verbatim, ask them to open it and approve, then retry. |
| `DEVICE IDENTITY CONFIRMATION REQUIRED` | First contact with this device: nothing was sent. Pin what it presented (`wanctl trust server --target … --fingerprint …`, or the wanctl_trust_server tool) and retry. |
| `result unknown: the connection dropped` | The request reached the device; the answer did not come back. It may or may not have been applied — read the file and compare its sha256 before retrying, rather than repeating the operation blindly. |
| `does not support read/edit; run `wanctl update`` | The device is running a wanctl older than the file tools. Update it there, then retry. |

## `wanctl write` / `wanctl_write`

*Create or completely replace a text file on a device*

Create a text file on a remote device, or replace one end to end, with the
content you pass inline. Use write only for NEW files or COMPLETE rewrites;
for changes to an existing file use wanctl_edit, which leaves the rest of the
file untouched and cannot silently drop someone else's change. Missing parent
directories are created. An existing file keeps its mode — a 0755 script stays
executable — and a new one gets 0644. The write is atomic (temp file +
rename), so a reader sees either the old file or the new one. Content is UTF-8
TEXT: 8 MiB at most, and bytes that are not valid UTF-8 are refused, because
they would not survive the trip. Binaries go through wanctl_push_blob (MCP) or
wanctl push (CLI) instead. If a call comes back 'result unknown: the
connection dropped after the request was sent', the device got it and the
answer was lost: read the file and compare its sha256 before retrying. Policy:
a write is the same grant as wanctl_push and wanctl_edit, so a first write to
an unapproved path may wait for the device owner to approve it.

**On the command line.**

--content takes the text directly and --content-file reads it from a local
file, which is how a multi-line file gets through without fighting the shell
over quoting. Giving both is an error rather than a precedence rule.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| `target` | `--target NS/DEV` | string | **yes** | Device ID or unique name/alias (DEVICE\|ALIAS), or NS/DEVICE\|NS/ALIAS for shared devices. |
| `path` | `<path>` | string | **yes** | Absolute path on the target device. `~` is NOT expanded. Parent directories are created if they do not exist. |
| `content` | `--content STR \| --content-file F` | string | **yes** | The whole new text of the file, UTF-8. An empty string is allowed and writes an empty file. Nothing is appended: whatever was there before is gone. |
| — | `--content-file F` | string | no | Read the content from this local file instead of --content. Giving both is an error, not a precedence rule. |

```
wanctl write --target lab /etc/app/config.toml --content-file ./config.toml
```

```
wanctl_write{"target":"lab","path":"/srv/run.sh",
    "content":"#!/bin/sh\nexec ./app\n"}
```

| Error | What to do |
|---|---|
| `not a UTF-8 text file` | The content is not text. Use push_blob (MCP) or push (CLI) for bytes; retrying write will not help. |
| `over the 8388608-byte write limit` | Too large for an inline write. Upload it with push, or split it. |
| `write denied by device policy` | The device owner has not granted write access to that path. |
| `PAIRING REQUIRED` | The device has not approved this controller yet. The message carries a URL valid for 5 minutes; give it to the user verbatim, ask them to open it and approve, then retry. |
| `result unknown: the connection dropped` | The request reached the device; the answer did not come back. It may or may not have been applied — read the file and compare its sha256 before retrying, rather than repeating the operation blindly. |
| `does not support write; run `wanctl update`` | The device is running a wanctl older than the write tool. Update it there, then retry. |

## `wanctl_exec_async`

*Start a background job and return its id at once*

Start work that will not finish inside one tool call, and get a job_id back
immediately. Reach for it BEFORE starting anything whose length you cannot
honestly predict — a package install, a build, a large download, a dev server
meant to stay up — because wanctl_exec holds the call open until the command
ends, and a call that times out loses both the output and the knowledge that
the thing is still running. Here the command keeps running on the device after
this returns; collect its output and exit code with wanctl_exec_poll(job_id).

It always runs in a FRESH shell. It does not inherit the working directory or
the exported variables of wanctl_exec's persistent session, so pass 'cwd'
explicitly instead of relying on a cd from an earlier call. Pairing, device
identity and policy are exactly as for wanctl_exec.

The ceilings are real: a job runs for at most 30 minutes, keeps at most 8 MiB
of output, and stays pollable for up to an hour after it ends, subject to the
device's overall retention budget. Anything that has to outlive those belongs
in something the device itself supervises — a service, a scheduled task —
started through this tool once.

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

Collect what a background job has produced since you last looked. Call it
after wanctl_exec_async and keep calling until state is 'done', which is also
the only point at which an exit code exists — before that a job has no result,
only output so far.

Pass the previous poll's 'next_offset' back as 'offset' so each call returns
only NEW output. Omit it or pass 0 only when you deliberately want everything
from the start; re-reading the beginning on every poll is how a long build's
output fills a conversation for no gain. The reply opens with a status header
— state running|done, the exit code once done, next_offset — and the output
follows it.

A poll that returns no new output is neither a failure nor a reason to start
the job again: it means nothing has been written since your last call. Wait,
do something else, and poll again. Starting a second copy of a build is worse
than waiting for the first.

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

Send a file that already exists on this machine to a path on the device. Reach
for it for bytes you cannot type out: a compiled binary, an archive, an image.
For anything expressible as text, prefer wanctl_write to create a file and
wanctl_edit to change one — this tool replaces a whole file and silently
discards whatever changed on the device since you last read it.

Available in stdio mode only. On a shared HTTP MCP server it is withdrawn,
because 'local' would name a path on the server rather than on the caller's
machine; there the equivalent is wanctl_push_blob with inline content.

Local paths are deliberately fenced: anything under a dot-directory of the
operator's home (~/.ssh, ~/.config and the like) is refused, and when
WANCTL_MCP_LOCAL_ROOT is set the tool cannot read outside that tree. Those
refusals are the operator's policy rather than a transient error — report them
and stop, do not go looking for another path to the same bytes. Pairing and
device policy are the same as for wanctl_exec.

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

Put bytes on the device when you have no file on the MCP server to send — the
normal case in HTTP (remote) MCP mode, where wanctl_push is withdrawn because
the AI host's files are not on the server. Encode what you want written as
standard base64 and pass it in 'content_b64'.

Reach for it for BINARY content. Text does not belong here: wanctl_write takes
the content directly with no encoding step, and a CHANGE to a file that is
already on the device belongs in wanctl_edit, because this tool overwrites the
file whole and anything edited since you last read it is gone without a word.

The cap is 8 MiB of raw (decoded) bytes; a larger body is refused, not
truncated. Past that, split the payload or have the device fetch the file
itself with wanctl_exec. Pairing and device policy are the same as for
wanctl_exec.

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

Bring a file off the device and keep it on this machine. Reach for it only
when you need the actual bytes locally — a binary, an archive, a log you will
hand to another tool. To LOOK at a text file, use wanctl_read instead: it
needs no local file, it reports line ranges and the whole file's sha256, and
it works on a shared HTTP MCP server where this tool is unavailable.

stdio mode only, and the same local-path limits as wanctl_push apply:
dot-directories of the operator's home are refused and WANCTL_MCP_LOCAL_ROOT
confines the tool to one tree. Pairing and device policy are the same as for
wanctl_exec.

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

Find out what actually happened on a device, as the device itself recorded it.
Reach for it when a call was refused and you need to know whether the owner
ever saw the request, when the user asks what a controller did to their
machine, or when an exec's own output does not explain its outcome. Every
connect, exec and file operation is one JSONL event carrying the policy
decision and the exit code, which is where an approval that was granted — or
quietly never was — becomes visible.

This is the LOG, not live state. It will not say whether a device is online
now (wanctl_peers) or whether a background job is still running
(wanctl_exec_poll). Narrow with 'type', 'grep' and 'since' rather than pulling
everything and reading it here, and note that 'limit' keeps the most recent
matches rather than the first.

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

Read the portal's or the relay's own process log, for the one question a
device's log cannot answer: whether the failure is on the server side at all.
Reach for it when calls fail for every device rather than one, or when a
pairing a user swears they approved never seems to arrive — not for auditing
what a controller did to a device, which is wanctl_logs.

It is gated on WANCTL_ADMIN_SECRET in the server's environment. Without it the
call is refused and retrying changes nothing: say so and move on, because an
ordinary user is not meant to have it. Output is redacted before your filter
runs, so a 'grep' for a token or a code finds nothing even when the line is
there — filter on the surrounding words instead. Keep 'since' short; the
default lookback is 15 minutes and the hard cap is 2000 lines.

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

Show THIS controller's own fingerprint — the string a device owner sees on the
approval screen and pins. Reach for it when a user is standing at their device
deciding whether to approve a pairing and wants to check that the fingerprint
in front of them is yours, or when a device lists a trusted controller and the
question is whether that is this session.

It is about this side of the connection only. The DEVICE's fingerprint, the
one you pin with wanctl_trust_server, is a different string and never comes
from here — it arrives inside the DEVICE IDENTITY CONFIRMATION REQUIRED
message.

```
wanctl id
```

```
wanctl_id{}
```

## `wanctl trust` / `wanctl_trust`

*List the trust store*

List what this session has already decided to trust, which is how you tell
first contact from a changed identity before acting on either. 'servers' (the
default) is the devices whose identity this session has pinned: a target that
is missing from that list will raise DEVICE IDENTITY CONFIRMATION REQUIRED on
the next call and you answer it with wanctl_trust_server, while a target that
IS in the list and raises DEVICE IDENTITY MISMATCH has changed under you and
is a matter for the user, not for another tool call.

'clients' is the other direction — controllers this machine has allowed to
drive it — and only means anything in stdio mode on a machine that is also
running wanctl agent. On a controller-only host, and on an HTTP MCP server, it
is empty, and that emptiness is normal rather than a symptom.

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

Pin what a device presented on first contact, so that a later change in its
identity can be detected. Call it as soon as any tool returns DEVICE IDENTITY
CONFIRMATION REQUIRED, passing the target and fingerprint copied VERBATIM out
of that result, then retry the call that failed. Do not stop to ask the user
to confirm the fingerprint first: your MCP client's own approval prompt is
already the human checkpoint, and on first contact there is nothing to compare
the fingerprint against anyway. This records what the device presented right
now; that is its whole purpose, and it is a one-time step per device.

'DEVICE IDENTITY MISMATCH' is the opposite situation, and this tool is the
wrong answer to it. There the device presented something other than what is
pinned — a reinstall, or someone standing in the middle. Do NOT call this
tool: report both the pinned and the presented fingerprint to the user and
stop. Re-pinning is a decision a human makes at a terminal with `wanctl trust
server --replace`.

The handler refuses outright unless the operator has set
WANCTL_MCP_ALLOW_UNSAFE_TRUST_SERVER=1. The hosted endpoint sets it; a local
stdio server usually does not, and there the way forward is to tell the user
to run `wanctl trust server` themselves rather than to retry.

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

List the policy THIS machine enforces on controllers that dial into it — what
the agent running here allows without stopping to ask a human. Read it when
you are working on the device side of the connection and want to know why a
controller is being refused.

It says nothing about what a remote device will let you do. A 'denied by
device policy' answer from exec, read, write or screenshot comes from that
device's own rules, which live on that device and cannot be read from here;
the way through is its owner approving the pending request, not a call to this
tool.

Only meaningful in stdio mode on a machine that is also running wanctl agent.
For a controller-only host and for an HTTP MCP server the list is empty, and
that is not a fault to investigate.

**On the command line.**

The CLI also writes: `wanctl rules add` appends a rule and `wanctl rules rm`
removes one. Rules are enforced by the agent on THIS machine, so they only
mean anything where a device runs.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| — | `--kind exec\|exec-elevated\|read\|write\|logs` | string | no | What the rule governs. `exec-elevated` is its own class: an `exec` rule never authorizes the elevated form of the same command, and on a device where bypass mode is on but the elevation channel is off this is the only way to pre-authorize one. `wanctl rules add` only. |
| — | `--pattern P` | string | no | For exec and exec-elevated, a command prefix with an optional trailing *; for a command sent with --script, the `script:<interp>:<sha256>` token the device names it by (a refusal prints it in full; approval cards abbreviate it). For file kinds, a directory. `wanctl rules add` only. |
| — | `--dir D` | string | no | For an exec or exec-elevated rule scoped to a working directory. `wanctl rules add` only. |

```
wanctl rules
  wanctl rules add --kind exec --pattern "git *"
  wanctl rules add --kind exec-elevated --pattern "pm install *"
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

## `wanctl screenshot` / `wanctl_screenshot`

*Capture a device's screen as a PNG*

Look at what is on a device's screen. This is the harness's eyes for anything
a command cannot tell you: a dialog waiting for a click, a desktop app's
state, a phone mid-flow, whether the thing you just started actually came up.
Works on Android (screencap through the elevation channel), macOS
(screencapture), Windows (the whole virtual screen, every monitor, through
.NET) and Linux (grim, gnome-screenshot or ImageMagick's import — the first
one installed; if none is, the error names them so you can install one).
Captures the whole screen: there is no window picker and no region.

Policy: a desktop capture is gated exactly like any other command, because it
is one — a controller allowed to run commands could run the capture tool
itself. On a desktop in BYPASS mode that means a capture is auto-approved like
any other command, with no separate prompt — if the device's owner does not
want that, the device should not be in bypass. Android is gated as an ELEVATED
command, because there a capture really does need su or the device's own adb:
that class needs its own rule or an approval, unless the phone is BOTH in
bypass mode and has its elevation channel switched on, in which case it is
auto-approved like any other command. Same pairing and identity rules as
wanctl_exec: 'PAIRING REQUIRED' carries a URL to relay VERBATIM to the user,
and 'DEVICE IDENTITY CONFIRMATION REQUIRED' means call wanctl_trust_server
with the target and fingerprint it gives you, then retry.

**On the command line.**

`screencap -p` writes a PNG to stdout, and through a shell pipeline stdout is
a terminal — a screenful of binary. The CLI writes a file instead, and only
writes to stdout when explicitly asked with `-o -`.

**As an MCP tool.**

The PNG comes back as image content, with a line of text giving its dimensions
and size. Captures over 4 MiB are downscaled to fit — the text line says so
and names the format — because the point is for you to SEE the screen, not to
archive it. If you need the original bytes, capture to a file on the device
with wanctl_exec and fetch it.

| Parameter | CLI | Type | Required | Meaning |
|---|---|---|---|---|
| `target` | `[DEVICE] \| --target NS/DEV` | string | **yes** | Device ID or unique name/alias (DEVICE\|ALIAS), or NS/DEVICE\|NS/ALIAS for shared devices. On the CLI it may also be the first positional argument. |
| — | `-o FILE` | string | no | Local file to write. Defaults to screenshot-<device>-<time>.png; "-" writes the PNG to stdout instead, which is the only way to pipe it. |
| `via` | `--via su\|adb` | string | no | Android only: pin the elevation channel, 'su' (rooted device) or 'adb' (the device's own wireless debugging). Default empty lets the device pick, and it is ignored by a desktop, which needs no channel. |

```
wanctl screenshot home-phone -o ./screen.png
```

```
wanctl_screenshot{"target":"home-pc"}
```

| Error | What to do |
|---|---|
| `PAIRING REQUIRED` | The device has not approved this controller yet. The message carries a URL valid for 5 minutes; give it to the user verbatim, ask them to open it and approve, then retry. |
| `DEVICE IDENTITY CONFIRMATION REQUIRED` | First contact with this device: nothing was sent. Pin what it presented (`wanctl trust server --target … --fingerprint …`, or the wanctl_trust_server tool) and retry. |
| `command denied by device policy` | The device has not allowed this controller to capture its screen. Ask the owner to approve the pending request, then retry. On Android the refusal names an ELEVATED command, which needs its own exec-elevated rule or an approval; bypass mode alone covers it only on a phone whose elevation channel is also switched on. |
| `no screen capture tool on this device` | A Linux device with none of grim / gnome-screenshot / import installed. Install one (the message names them) — retrying will not help. |
| `screencapture failed: … create image from display` | macOS withheld the screen: the agent has no Screen Recording permission. Open System Settings → Privacy & Security → Screen Recording on that Mac, add the wanctl binary (or the app that launched the agent), turn it on, then restart the agent — retrying without that will not help. |
| `did not return a PNG` | The device answered with something else, usually an agent too old for desktop capture. Run `wanctl update` on it, then retry. |

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

