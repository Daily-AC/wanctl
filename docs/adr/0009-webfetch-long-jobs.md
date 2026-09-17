# 0009 — WebFetch long jobs: raise the ceiling, not the transport

Date: 2026-09-17
Status: accepted

## Context

The first real WebFetch user (Qwen 3.8-Max, 2026-09-17) got through
authorization and then asked the device to run a Blender modelling job. The call
came back `context deadline exceeded` with the job in `unknown`: the human could
not tell whether Blender had run.

Two separate ceilings produced that.

`parseOperation` capped `timeout_seconds` at 60 (`internal/webfetch/handler.go`),
and the adapter turns that number into the job's whole budget:
`deadline := task.job.CreatedAt.Add(timeout)`, queue time included. A ten-minute
render never had a chance.

`jobResponse` then downgraded any job still `running` after a fixed 90 seconds to
`unknown`. That constant knew nothing about what the caller had asked for, so
even if the timeout had been raised, a healthy long job would have been reported
as an ambiguous side effect roughly a minute and a half in.

A third pressure: four worker goroutines drained one shared queue. With
sixty-second jobs that was invisible. With thirty-minute jobs, four long
operations from one grant would stall every other user's `read_text`.

## Decision

Keep the synchronous one-shot wanctl operation and raise the ceilings.

**`exec` gets 1–1800 seconds, default 300. File tools stay at 1–60, default 30.**
The number is per tool because the tools differ in kind: an `exec` is whatever
device policy already allows, including builds and renders, while `read_text`
and `write_text` move at most 32 KiB — one that is slow is stuck, and it must not
hold a slot for half an hour. The default moved from 30 to 300 for `exec` because
the observed failure was a caller who did not set the parameter at all: a
too-large default fails slowly and visibly, a too-small one kills the job and
leaves the device state ambiguous.

**An omitted `timeout_seconds` stays omitted from the canonical payload.** The
payload is the replay identity: a lost response is recovered by fetching the
identical URL, which must hash to the same job. If the default were baked in,
moving it from 30 to 300 would change that hash and turn a legitimate transport
retry into a 409. The default is applied at dispatch only; an explicitly supplied
value stays in the hash and still conflicts when it changes. Jobs created before
this deploy did record the old default, so a pre-deploy rid replayed with no
`timeout_seconds` will 409 once. Grants live at most 60 minutes, so that window
closes by itself.

**`unknown` is now derived from the job's effective deadline** — the earlier of
the requested timeout and the end of the grant, one value shared by execution,
the advertised `deadline_at` and the stale-state inference. `jobState` reports
`unknown` only after `CreatedAt + timeout_seconds + staleGrace` (90 s). Running
jobs also return `poll_after_seconds` and `deadline_at`, so the client waits
instead of burning the old "at most 8 polls" budget in half a minute.

**The fixed worker pool is gone.** Each operation runs on its own goroutine under
two counters: at most 4 in flight per grant, 64 across the adapter. One grant's
long job can no longer starve another, and the global counter still bounds the
process. A call over either limit is refused **before** the ledger write, with
`error_code: "adapter_busy"` and `execution_started: false`, so a refusal never
spends the caller's 64-job allowance on an operation that did not run. The slot
is released by a deferred call around `execute`, which covers a result, a
transport failure, a store write that fails and a recovered panic alike.

A per-owner budget between those two was written and then removed: the relay is
moving to a single-owner deployment (`wanctl-relay-cf-tunnel-plan`, 2026-09-17),
so one account starving another is not a problem this code needs to solve. If
the relay ever becomes multi-tenant again, that is the gap to reopen — a grant
is not a person, and one account can hold many grants at once.

## Why not run exec through `exec_async` / `exec_poll`

That was the preferred option: the device already keeps background jobs alive
across connections (`internal/agent/jobs.go`), `internal/client/exec_async.go`
already exposes `ExecAsync` and `ExecPollTo`, and `internal/mcp/server.go` uses
them for `wanctl_exec_async`. A long job would then not hold any adapter slot at
all, and a dropped relay connection would stop turning into `unknown`.

The device refuses it. `internal/agent/agent.go:722`:

```go
if check != nil && (m.Kind == protocol.KindExecAsync || m.Kind == protocol.KindExecPoll || (m.Kind == protocol.KindExec && !m.OneShot)) {
	... "delegated execution requires a synchronous one-shot command"
```

`check` is non-nil exactly when the session carries a grant
(`internal/agent/agent.go:568`), and every WebFetch session does. The refusal is
deliberate: a delegated controller gets commands the device can attribute, gate
and end with the session, never a detached process that outlives the grant it was
authorized under. Reaching the async path would mean weakening that boundary on
the device, which is a different decision from raising a timeout, and it belongs
to whoever owns the delegation model — not to this change.

Even with the boundary relaxed, the ledger could not follow: `FinishJob` accepts
only `done`, `failed` and `unknown` and only from `running`
(`internal/relay/delegation_store.go`), so a device-side job id has nowhere
durable to live. Async execution would need a job-store change too.

## Consequences

- A thirty-minute exec works, but only inside a grant that is long enough: the
  deadline is still clamped to `access.ExpiresAt`, and a grant is at most 60
  minutes. The discovery procedure therefore tells the model to ask the human for
  a duration longer than the task.
- An adapter restart still loses in-flight jobs; they read `unknown` after their
  deadline rather than after 90 seconds. Recovering them needs the async path
  above, so this trade is unchanged, just slower to appear.
- A grant that wants more than four operations at once now gets a clean refusal
  instead of silent queueing behind someone else's render.
- Cross-account isolation is out of scope while the deployment is single-owner.
  Sixteen grants of four long operations each can still take the whole adapter;
  on a shared relay that would be a starvation channel.
- `unknown` no longer names a cause, because the adapter records it for four
  different ones. The instruction is the same in all of them: do not repeat the
  operation automatically, check the device.
