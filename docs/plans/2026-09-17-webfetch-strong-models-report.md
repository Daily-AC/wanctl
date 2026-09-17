# WebFetch for strong models — one prompt, and jobs that outlive a minute

Branch `feat/webfetch-strong-models`, 2026-09-17.

Two changes: the machine-facing text now reads as a procedure a capable model
executes with two named human checkpoints, and an `exec` may run for up to 30
minutes without ending in `unknown`.

## Files changed

| File | What |
| --- | --- |
| `internal/webfetch/handler.go` | Per-tool timeout ceilings; deadline-aware `unknown`; per-job goroutines under per-grant slots; shorter discovery/pending/pairing text; HTML template renders the procedure and checkpoints |
| `internal/webfetch/manifest.go` | `discovery()`, `humanCheckpoints()`, `securityRules()`; shorter `continuation_prompt` plus a Chinese twin; per-tool timeout schema; new `limits` keys |
| `internal/webfetch/discovery_test.go` | New. Checkpoint/security-key contract, page word budget, `jobState` boundary — all run without PostgreSQL |
| `internal/webfetch/handler_e2e_test.go` | New full-flow test through both checkpoints; new long-exec and per-tool-ceiling tests; `pair` helper; `sleep 2` in the fixture's policy |
| `internal/portal/web/webfetch-connect.html` | Rebuilt around the one-line prompt (中文 + English), a 3-step "what happens next", fallback prompt demoted below |
| `internal/portal/web/webfetch-help.html` | Retrimmed around the two named checkpoints; defensive repetition removed |
| `internal/portal/web/app.css`, `auth.js` | `.say` block styles; a generic `[data-copy]` copy button with a select-the-text fallback |
| `internal/portal/webfetch_test.go` | Asserts the one-line prompt in both languages, above the fallback |
| `docs/webfetch.md`, `docs/webfetch.zh.md` | Limits paragraph, `exec` table row, polling/`unknown` semantics |
| `docs/adr/0009-webfetch-long-jobs.md` | New |

## Criteria

| # | Criterion | Result |
| --- | --- | --- |
| 1 | Full flow test with a fake device | `TestWebFetchOnePromptFlowThroughBothHumanCheckpoints` — discovery → `new/{nonce}` → approve → manifest → exec returns `pairing_required` (asserts `pairing_url`, the named Step 2 of 2 checkpoint and the `NEW rid` instruction) → pair → exec under a new rid returns `done` with `exit_code` 0 |
| 2 | Long exec ends `done`, others run meanwhile | `TestWebFetchLongExecFinishesAndDoesNotBlockAnotherGrant` (real device, `timeout_seconds=900`, another grant's job completes during it) plus `TestLongJobIsNotReportedUnknownBeforeItsOwnDeadline` for the clock boundary. See the caveat below |
| 3 | Discovery JSON names both checkpoints and keeps the security rules | `TestDiscoveryNamesBothHumanCheckpointsAndKeepsSecurityRules`, asserting on `human_checkpoints[].id/name/url_field` and the eight `security.*` keys |
| 4 | Rendered `/webfetch/v1` under 2,500 words per language | **985 Latin words, 104 Han characters** after fix round 1 (928 before the added security rules). Pinned by `TestDiscoveryPageStaysShortEnoughToRead` |
| 5 | `/webfetch/connect` one-line prompt above the fold at 390×844 | Yes. Measured, see below |
| 6 | `go vet ./...`, `gofmt -l .`, the two test packages | All clean; the whole `go test ./...` is green with PostgreSQL |
| 7 | Live fetch of `/webfetch/v1` | Yes, via `go run ./tools/webfetch-demo` |

### Criterion 2, honestly

The spec asked for a fake device that takes 120 s. The end-to-end fixture drives
a real agent, so a 120-second sleep would put two minutes into every CI run. It
is split instead:

- the clock rule is a table test on `jobState(state, age, timeout)` — age 120 s
  against a 900-second timeout stays `running`, which is exactly what the old
  fixed 90-second constant got wrong;
- the device half uses `sleep 2` with `timeout_seconds=900`, which the old code
  rejected outright at parse time (max 60), and asserts the job never reads
  `unknown`, ends `done` with its exit code, and that a second grant's `exec`
  completes while it runs.

`staleGrace` is a package variable so a test can compress the scale further if a
future case needs to.

### Criterion 5, how it was measured

Chrome headless on macOS silently clamps `--window-size` to about 500 CSS px, so
the first screenshot was a 500-wide layout cropped to a 390-wide image and looked
broken. Real measurement uses CDP `Emulation.setDeviceMetricsOverride` at
390×844, dpr 3, against the live demo portal:

```
{"vw":390,"scrollW":390,"overflow":[],
 "zhBottom":282,"enBottom":407,"copyButtons":[282,407],
 "stepsBottom":593,"fold":844}
```

No horizontal overflow, both prompts and both copy buttons above the fold, the
3-step list ending at 593 of 844. Checked in both languages.

### Criterion 7, first 40 lines of the live page

`WANCTL_TEST_POSTGRES=… go run ./tools/webfetch-demo --state-dir … --public-origin http://127.0.0.1:18995`,
then `curl -s http://127.0.0.1:18995/webfetch/v1`, tags stripped:

```
wanctl WebFetch
Run commands and read or write text files on the human's own devices, using only your URL-reading tool. You lead; the human acts twice.
Procedure
1. Generate a client_nonce in client_nonce_format and substitute it into start_url_template. Never fetch the literal template.
2. GET that complete URL. Check that the response echoes your client_nonce; a different value means you read a cached response, so start over with a new nonce.
3. Step 1 of 2: device access. Show the human approval_url and continuation_prompt, and ask them to approve a duration longer than the task needs. Then stop and wait.
4. GET status_url until status is approved. If it is rejected or expires, say so and stop.
5. Read the approved manifest at status_url: devices[].target, call_endpoint and tools[].call_url_template.
6. Fill one call_url_template with URL-encoded values and GET it. Then follow next_url until the status is done, failed or unknown. A long exec is normal; wait poll_after_seconds between reads.
7. Step 2 of 2: pairing. Only if a result has error_code pairing_required: show the human pairing_url, wait for them to confirm they approved it, then retry the same operation with a NEW rid.
8. Report the actual result: for exec, stdout, stderr and exit_code. Keep status_url and the exec call_url_template in your reply so a later turn can continue.
The two things the human does
Step 1 of 2: device access — Open this link, pick the devices you want me to use and how long, and approve. Choose a duration longer than the task will take. I will wait. (link: approval_url)
Step 2 of 2: pairing — The device has not paired with me yet and nothing ran. Open this link and approve the pairing, then tell me it is done and I will run the command again. (link: pairing_url)
help_url   http://127.0.0.1:18996/webfetch/help
entry_url  http://127.0.0.1:18995/webfetch/v1
owner_start_url  http://127.0.0.1:18996/webfetch/connect
Protocol response
{
"authorization": "wanctl device scope, expiry, revocation, identity trust and device-local policy all still apply. This protocol adds no permission of its own.",
"client_nonce_format": "48 lowercase hexadecimal characters from 24 fresh cryptographically random bytes",
"entry_url": "http://127.0.0.1:18995/webfetch/v1",
"help_url": "http://127.0.0.1:18996/webfetch/help",
"http_status": 200,
"human_checkpoints": [
{
"id": "device_access",
"name": "Step 1 of 2: device access",
"name_zh": "第 1 步（共 2 步）：设备访问",
"of": 2,
"step": 1,
"summary": "Open this link, pick the devices you want me to use and how long, and approve. Choose a duration longer than the task will take. I will wait.",
"summary_zh": "请打开这个链接，选择要让我操作的设备和授权时长，然后批准。时长请留得比任务本身长一些。我在这里等你。",
"url_field": "approval_url",
"where": "portal"
},
{
"id": "pairing",
```

## The long-job option, and why

Option (a): raise the ceilings, keep the synchronous one-shot operation, add a
per-grant concurrency slot. The reasoning is in `docs/adr/0009-webfetch-long-jobs.md`.

Option (b), routing `exec` through `exec_async`/`exec_poll`, is blocked on the
device, not on `internal/client`. `internal/agent/agent.go:722` refuses both
kinds for any session that carries a grant, with
`"delegated execution requires a synchronous one-shot command"`, and `check` is
non-nil exactly when `auth.GrantID != ""` (`internal/agent/agent.go:568`), which
is every WebFetch session. That refusal is a delegation boundary, not an
oversight, and `internal/agent` is outside this wave. A second blocker sits
behind it: `FinishJob` accepts only terminal states from `running`
(`internal/relay/delegation_store.go`), so a device-side job id has nowhere
durable to live.

`exec`: 1–1800 s, default 300. `read_text`/`write_text`: unchanged at 1–60,
default 30. Concurrency: 4 per grant, 64 across the adapter, each operation on
its own goroutine instead of a 4-worker shared queue.

## Deviations and what was left out

**`continuation_prompt` kept its job.** The spec described it as the one or two
sentences shown to the human at each checkpoint. It is also the cross-turn resume
text, and `contract_e2e_test.go:58,125` asserts it carries the full `status_url`
and the words `call_url_template`. Making it a human-facing sentence would drop
the only thing that lets a truncated conversation resume from a pasted line. So
`continuation_prompt` is now one short resume sentence (plus
`continuation_prompt_zh`), and the sentence to read aloud to the human lives on
`human_checkpoint.summary` / `summary_zh`, which the instruction text points at
by name.

**The page the owner sends to a friend is `/webfetch/help`, not
`/webfetch/connect`.** The task brief equated `/webfetch/connect` with
`webfetch-help.html`; in the code `/webfetch/connect` renders
`webfetch-connect.html` behind `pageAuth` (`internal/portal/webfetch.go:35`) and
is the owner's own page, while `/webfetch/help` is the public credential-free
one. The one-line prompt went on `/webfetch/connect` as criterion 5 requires.
`webfetch-help.html` was retrimmed to the same two-checkpoint model but stays the
AI's quick reference, because that is what a friend's AI actually reads.

**Language mechanism kept as found.** Portal pages use `data-en`/`data-zh` with
the `#lang` chip and a `wanctl.lang` localStorage key (`auth.js`). The relay
documents are English with a Chinese twin for every sentence meant for a human
(`summary_zh`, `continuation_prompt_zh`). No `Accept-Language` negotiation was
added; there is none today.

**`MaxRequestTime` deleted.** It was an exported constant with no reader
anywhere in the repo.

**Left out, on purpose:** nothing in `internal/agent`, `internal/client`,
`internal/mcp`, `internal/protocol` or the root CLI. No new environment
variables. Tool names and parameter names unchanged.

**Cut candidate for a later wave:** the personal "connection prompt" on
`/webfetch/connect` and the `owner_start_url` path it serves exist only for
clients that cannot generate 24 random bytes. Every target model can. Removing
both would delete a textarea, ~20 lines of `auth.js`, and one branch of the
discovery document. It was kept this round because the brief asked for the
existing details to stay below the new first screen.


## Fix round 1 — cross-vendor security review of PR #92

| Finding | Change | Test |
| --- | --- | --- |
| F1 Owner-level concurrency isolation missing; `adapter_busy` also spent the victim's ledger allowance | Third counter: 4 per grant, **8 per owner namespace**, 64 per adapter. The slot is reserved **before** `BeginJob`, so a refusal writes nothing and returns 429 with `adapter_busy` and no `job_id`. Release is a `defer` around `execute` in the dispatch goroutine, covering a result, transport failure, store-write failure and recovered panic; the request path releases on a duplicate rid and on a `BeginJob` error | `TestOperationSlotsAreBudgetedPerOwnerAndAlwaysReleased`, `TestWebFetchBusyAdapterRefusesWithoutTouchingTheLedger` |
| F2 Identical-URL replay changed identity across the deploy | `Operation.Timeout` is `*int` with `omitempty`. An omitted `timeout_seconds` never enters the canonical payload; the default is applied at dispatch through `Operation.timeout()`. An explicit value stays in the hash and still 409s when changed | `TestOmittedTimeoutIsNotPartOfTheReplayIdentity`, `TestWebFetchIdenticalURLReplaysTheSameJobAcrossDefaultChanges` |
| F3 Approved JSON manifest lost the same-rid retry rule | `security` (all ten rules) is now in the approved manifest, not only discovery. `rid_unique_per_operation` split into `retry_a_lost_response_on_the_same_url` and `new_rid_only_when_nothing_ran`. The manifest `instruction` and both continuation prompts say it too | `TestApprovedManifestCarriesTheRetryRules` |
| F4 `deadline_at` ignored the grant clamp | One `effectiveDeadline(created, timeout, grantExpiry)` drives execution, `deadline_at` and `jobState` | `TestWebFetchDeadlineIsClampedToTheGrant`, plus the grant-clamp row in `TestLongJobIsNotReportedUnknownBeforeItsOwnDeadline` |
| F5 `unknown` blamed the deadline for four different causes | One `unknownInstruction` constant, cause-neutral, used by `jobResponse`, the transport-failure path and the recovered panic. `docs/webfetch.md` and `docs/webfetch.zh.md` updated | `TestUnknownDoesNotBlameTheDeadline`, `TestWebFetchInterruptedJobDoesNotBlameTheDeadline` |

Test seams added: `MaxExecSeconds`, `DefaultExecSeconds`, `MaxFileSeconds`,
`DefaultFileSeconds` and the three concurrency caps are now package variables,
alongside the existing `staleGrace`. Nothing outside the package assigns them.

**Not fixed, as agreed:** `execute` discards a known exit code when `FinishJob`
fails (`internal/webfetch/handler.go`, the deferred writer logs and drops it).
Pre-existing, unchanged by this branch.

**Per-owner budget lives in the adapter, not the grant store.** The relay still
lets one account hold any number of approved grants
(`internal/relay/delegation_store.go`); capping that is a separate decision about
what an owner may approve, and `internal/relay` was not in scope for this wave.
