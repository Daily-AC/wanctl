# CLI help as a thin MCP — 2026-09-17

## What changed

`wanctl` had two descriptions of itself. `printUsage` in `main.go` was a
sixty-line wall of every flag of every subcommand, and `wanctl exec -h` was Go's
raw flag dump. Meanwhile the MCP tool descriptions in `internal/mcp/server.go`
were written for a reader who has to decide something: when to reach for the
tool, what each argument means, what each error text obliges the caller to do.
A ChatGPT user did not know wanctl already had persistent sessions and
background jobs, because only the MCP descriptions said so.

There is now one catalog. `internal/catalog` holds one value per command — CLI
spelling, MCP spelling, summary, description, parameters with per-surface
wording where the two genuinely differ, an example per surface, and the error
texts a caller must branch on. Three readers consume it and none keeps a copy:

- `registerMCPTools` builds every `mcpapi.NewTool` call from it.
- `wanctl help <command>` and `wanctl <command> -h` render the entry.
- `docs/contract.md` is `wanctl help --markdown`, committed, with a test that
  fails when it drifts.

Bare `wanctl` now prints a short index instead of the wall: one line per
command, grouped, under thirty lines and eighty columns, plus the local status
line it always had, plus a pointer to `wanctl help`.

## What these descriptions are

The owner's framing, which heads the contract and the package doc: wanctl is the
external harness for a web AI. The AI in the chat window is the brain; wanctl
gives it hands, eyes, memory across turns and safety rails, and together they
form one agent.

That has a consequence for how entries are written. What an MCP host loads from
this catalog is that agent's system prompt — read once, before any work, by the
thing about to act. So entries are operating instructions, not reference prose:
when to reach for this and not that, what an error obliges you to do next, what
to do before you start. The last of those is explicit in the DEV LOOP paragraph
on `wanctl_exec`, and again on `wanctl_read`: before working inside a project
directory, read its AGENTS.md or CLAUDE.md with `wanctl_read` if one exists, and
follow it. A test pins the rule so it cannot be dropped silently.

One claim, one source. `catalog.Headline` is the sentence the index header, the
contract intro and the README all render or are checked against, so what wanctl
is cannot be updated in one place and go stale in the other two.

## Files changed

| File | What |
|---|---|
| `internal/catalog/catalog.go` | types, `Product`, `Lookup`, per-surface accessors |
| `internal/catalog/commands.go` | the catalog: 37 entries |
| `internal/catalog/render.go` | index, terminal entry, Markdown contract |
| `internal/catalog/render_test.go` | width budgets, lookup, index coverage, product framing |
| `internal/mcp/server.go` | `registerMCPTools` builds from the catalog |
| `internal/mcp/catalog_registration_test.go` | schema snapshot + must-keep phrases |
| `internal/mcp/testdata/mcp_registration.json` | the pre-change registration, captured from `main` first |
| `main.go` | index, `cmdHelp`, `withHelp`, `-h` routed before the relay gate |
| `*.go` (9 files) | every `flag.NewFlagSet` wrapped in `withHelp` |
| `help_catalog_test.go` | index budget, both spellings, unknown command, doc drift |
| `entrypoint_test.go`, `fileops_cli_test.go` | updated for the new index |
| `docs/contract.md` | generated |
| `README.md` | one link, one product line, pinned to `catalog.Headline` by a test |

## Criteria

| # | Criterion | Result |
|---|---|---|
| 1 | Every MCP tool still registered with identical name, parameter names, types, required flags | pass — snapshot captured from `main` before any change, asserted in `TestRegistrationMatchesSnapshot` |
| 2 | Must-keep phrases present in generated descriptions | pass — `TestDescriptionsKeepTheRules`, tool and parameter level, including the AGENTS.md rule |
| 3 | Bare `wanctl` ≤ 30 lines, ≤ 80 columns | pass — 29 lines, 77 columns (`TestIndexFitsTheBudget`) |
| 4 | `help exec`, `exec -h`, `help read`, `help wanctl_read` render; `help nosuch` exits non-zero with the index | pass — `TestHelpRendersEntriesForBothSpellings`, `TestHelpForUnknownCommandFails` |
| 5 | `docs/contract.md` in sync | pass — `TestContractDocIsInSync` |
| 6 | `go vet`, `gofmt -l`, `go test ./...` | pass — vet clean, gofmt empty, all packages green |
| 7 | Real run pasted below | done |

## Real output: bare `wanctl`

```
wanctl — the external harness for a web AI, over an encrypted relay
USAGE: wanctl <command> [flags]   ·   wanctl help <command>  explains one
 DEVICE LIFECYCLE (run on the machine you want to control)
  start       log in if needed, then run the agent in the background
  stop        stop the background agent
  status      local agent and credential state, or a remote device's
  service     install/uninstall/status an OS-native always-on service
  agent       run the agent in the foreground
 SESSION (run where you — or your AI — drive from)
  login       log in via the portal and save the token (no daemon)
  logout      stop the agent and forget the saved login
  peers       list the devices this token can reach
  pair        check trust, or get the URL the device owner approves
  id          show this controller's identity fingerprint
  trust       list pinned device identities, or trusted controllers
  rules       show or change this machine's local policy rules
 CONTROL
  exec        run a command or script on a device (persistent shell)
  logs        read a device's activity log, or portal/relay server logs
  screenshot  capture an Android screen to a local PNG
 FILES
  read        print a line range of a text file on a device
  edit        replace an exact string inside a file on a device
  push        copy a local file to a device
  pull        copy a file from a device to this machine
 MORE  update version mcp docs friends share config label admin portal-admins
       relay portal
Defaults: relay=(not set) portal=(not set)  (change with 'wanctl config set')
本机: 未登录 · agent 未运行
```

## Real output: `wanctl help exec`

```
wanctl exec — Run a command, or a whole script, on a device
MCP tool: wanctl_exec

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

  On the command line --target may be omitted when the first argument names a
  device, or when exactly one device is online. --script takes a path to a local
  file, not the source itself. Background jobs — the wanctl_exec_async and
  wanctl_exec_poll pair above — have no CLI spelling yet; from a terminal,
  background the command on the device instead.

PARAMETERS
  --target NS/DEV              string, required
      Device ID or unique name/alias (DEVICE|ALIAS), or NS/DEVICE|NS/ALIAS for
      shared devices. If exactly one device is online for this token, you may
      pass empty string.
  <command...>                 string
      The command to run, given as the trailing arguments. It is SOURCE CODE for
      the device's shell (sh on Unix, powershell on Windows) and is parsed
      there, so a nested `powershell -Command "...$x..."` is parsed twice and
      loses its variables to the outer pass; wanctl warns when it sees that
      shape. Use --script instead of nesting an interpreter.
  --script <local-file>        string
      Path to a script FILE on this machine. Its contents are sent
      base64-encoded and run on the device, so quoting and character-set rules
      do not apply: $, backticks, nested quotes and non-ASCII text all arrive
      literally. The interpreter comes from the extension (.ps1 → PowerShell,
      .sh or none → sh) unless --interp says otherwise. This is the CLI spelling
      of the MCP `script` argument, which carries the source itself rather than
      a path.
  --interp powershell|sh       string
      Override the interpreter --script would infer from the file extension:
      powershell | sh.
  --oneshot                    boolean
      Run in a fresh shell with no persistent session state. Default false —
      successive exec calls share cwd/env like a real terminal.
  --elevate                    boolean
      Android only. Run with elevated privilege (uid 0 or the adb shell uid
      2000) instead of the app sandbox the agent normally lives in. This is what
      makes `pm`, `am`, `input`, `screencap`, `dumpsys`, `settings`, `wm` and
      `svc` work at all — without it they fail with permission errors or empty
      output. Elevated commands need their OWN policy rule on the device; a
      device in bypass mode still refuses them until a human approves, so expect
      a 'PAIRING/approval' style rejection the first time.
  --via su|adb                 string
      Pin the elevation channel: 'su' (rooted device) or 'adb' (device's own
      wireless debugging). Default empty = let the device pick whichever is
      available. Naming an unavailable channel fails instead of quietly running
      unprivileged.

EXAMPLE
  wanctl exec --target home-pc "uname -a"
  wanctl exec --target home-pc --script ./setup.sh
  MCP: wanctl_exec{"target":"home-pc","command":"uname -a"}

ERRORS the caller must react to
  PAIRING REQUIRED
      The device has not approved this controller yet. The message carries a URL
      valid for 5 minutes; give it to the user verbatim, ask them to open it and
      approve, then retry.
  DEVICE IDENTITY CONFIRMATION REQUIRED
      First contact with this device: nothing was sent. Pin what it presented
      (`wanctl trust server --target … --fingerprint …`, or the
      wanctl_trust_server tool) and retry.
  DEVICE IDENTITY MISMATCH
      The device presented a different identity than the pinned one. Refused;
      nothing was sent. Report both fingerprints and stop — re-pinning is a
      human decision at a terminal.
  LOGIN REQUIRED
      No usable credential. Run `wanctl login` (CLI) or wanctl_login (MCP); an
      MCP session that had one can restore it instantly with
      wanctl_login(rebind=…).
```

## Left out, and why

- **No CLI spelling for background jobs.** `wanctl_exec_async` and
  `wanctl_exec_poll` have no `wanctl` subcommand, and adding one would be a
  behaviour change. They are full catalog entries and appear in the contract;
  `wanctl help exec` says so in its CLI note.
- **Portal docs untouched.** `docs/portal/ai__*.md` (both languages) contain no
  sentence describing the help output, so per the brief they were left alone.
- **`wanctl exec help` is still a command, not a request for help.** Only `-h`,
  `-help` and `--help` are intercepted before the relay gate; `help` is a
  plausible thing to run on a device.
- **The other seventeen descriptions were not rewritten as instructions.** The
  framing above says how the next entry gets written and it shaped the new text
  (the dev loop, the START OF TASK rule, the error tables). Rewriting the
  inherited prose tool by tool is a separate pass; the must-keep test exists so
  that pass cannot quietly drop a rule.
- **The 256 KiB read cap, the 8 MiB edit cap and the 30-minute job ceiling are
  described, not enforced here.** The catalog quotes the limits the handlers
  already apply; nothing about behaviour moved.
