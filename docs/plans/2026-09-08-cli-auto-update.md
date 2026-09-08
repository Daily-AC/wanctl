# CLI auto-update

Status: in progress (2026-09-08). Branch `feat/cli-auto-update`.

## Why

Every fix that needs the client side (v0.6.0 device identity, v0.6.1 Android
device ID) reaches a device only after someone runs `wanctl update` on it.
Devices that run unattended — a headless box under `wanctl service`, a laptop
that someone enrolled once — never get it. The owner has to message every
device owner and ask them to run a command. An agent that is already a
long-running process can do this itself.

## Scope

In: `wanctl agent` on darwin, linux and windows, whether started by `wanctl`
/ `wanctl start` (detached, pid file) or by `wanctl service` (systemd,
launchd, Scheduled Task with `__supervise`).

Out, and stays out:

- Android app. The binary inside an APK cannot be swapped (`runningFromAPK`);
  the app already has "检查更新" through the package installer.
- Relay / portal containers. Deployed by hand with a database backup.
- Development builds (`buildVersion == "dev"`). A developer's own binary must
  never be replaced under them.
- Rollback after a successful swap. The signed release pipeline is the gate;
  a bad release is fixed by shipping the next one, which the same mechanism
  delivers.

## Behaviour

**Setting.** One switch, `auto_update`, values `on` / `off`, default `on`.
Persisted with `wanctl config set auto_update=off`, overridable with
`WANCTL_AUTO_UPDATE` (env wins, as for every other setting). Read at every
check, so switching it off takes effect without restarting the agent. No
interval setting, no channel setting.

**Schedule.** First check 60 s after the agent starts, then every 6 h plus a
random jitter of up to 30 min. A check that finds the agent busy retries in
5 min. A check that fails (network, verification) logs the error once — the
same error text is not repeated on consecutive ticks — and waits for the next
regular tick.

**Source.** Same as `wanctl update`: the persisted / baked `release_base`,
else the relay's `/dl` mirror. New for both the manual and the automatic
path: when `release_base` is set and fetching or verifying the manifest there
fails, fall back to the relay's `/dl` mirror if it is a different URL. The
mirror serves the same signed manifest, so trust does not change. A manifest
that verifies but offers nothing newer (`ErrUpToDate`) is a result, not a
failure, and does not trigger the fallback.

**Decision, in order, at every check.**

1. setting off → skip, nothing logged.
2. `buildVersion == "dev"` → skip.
3. `runningFromAPK(self)` → skip.
4. Fetch and verify the manifest; `Select` for this OS/arch/version. Up to
   date → done.
5. Binary directory not writable (`canWriteDir` false, e.g. `/usr/local/bin`)
   → log once per newer version: `wanctl: 新版本 vY 可用，但 <dir> 不可写，
   自动更新无法进行；请手动运行 sudo wanctl update`. Never attempt sudo from
   the daemon.
6. Agent busy → retry in 5 min. Busy means any of: an open shell session
   (`sessions` entry not closed), a running async job (`jobs.running > 0`), an
   active console session. A relay that is currently unreachable is not busy.
7. Download to a tempfile in the binary's directory, verify, chmod 0755,
   `replaceBinary(tmp, self)` — the existing atomic swap. Then restart.

**Restart.**

- darwin / linux: after `Agent.Run` returns for this reason, `cmdAgent`
  releases the agent lock and calls `syscall.Exec(self, os.Args, os.Environ())`.
  Same pid, so the pid file, the managed-pid file, systemd's MAINPID,
  launchd's tracked process and the `__supervise` parent all stay valid.
  The lock file descriptor is close-on-exec, so the new image reacquires it.
  Go opens all files with `O_CLOEXEC`; stdout/stderr (fds 1, 2) are inherited,
  so the log file keeps receiving output.
- windows, managed (`config.ManagedPID() == os.Getpid()`): return cleanly
  with exit code 0. `cmdSupervise` re-runs the stable binary path after 3 s.
- windows, detached: release the lock and pid file, start
  `<self> agent <same args>` detached (same `SysProcAttr` and log file as
  `cmdStart`), then exit 0. The old `.exe` has already been renamed to
  `.old` by `replaceBinary`; the running process is unaffected.

**Logging.** All to the agent's own stdout/stderr (the log file):

- on start: `wanctl agent <version> 已启动` (add if not already present)
- on update: `wanctl: 自动更新 <old> → <new>，已验签，正在重启 agent`
- on failure: `wanctl: 自动更新检查失败: <err>` (deduplicated as above)

**`wanctl status`** (local, no target): one more line under the endpoints,
`  自动更新: 开启` or `  自动更新: 关闭`.

**`wanctl update`** (manual): unchanged apart from the source fallback.

## Files

- new `autoupdate.go` (package main): the loop, the decision function, the
  restart plumbing. `autoupdate_unix.go` / `autoupdate_windows.go` for the
  exec / respawn halves.
- `main.go` `cmdAgent`: start the loop next to `ag.Run`, handle the restart
  sentinel after `Run` returns.
- `internal/agent/agent.go`: `func (a *Agent) Busy() bool`, and whatever
  Run needs to return on request (a stop reason, or a context the loop
  cancels).
- `update.go`: `updateSources() []string` replacing `updateSource()`;
  `downloadSignedUpdate` callers iterate with the fallback rule above.
- `config_cmd.go`, `internal/config/store.go`, `internal/config/config.go`:
  the `auto_update` key, its validation, its env name.
- `daemon.go` `printLocalStatus`: the status line.
- `docs/environment.md`, `docs/environment.zh.md`: `WANCTL_AUTO_UPDATE` row.
- `README.md` / `docs/self-hosting*.md`: one short paragraph where `wanctl
  update` is documented.
- `docs/adr/0006-cli-auto-update.md`: default on, exec-in-place on Unix,
  no rollback, idle gate.

## Acceptance

Unit tests (implementer):

- decision table: off / dev / apk / up-to-date / unwritable / busy / go.
- loop against a signed httptest server (extend `signedUpdateServer` to take
  the artifact OS/arch and version): swaps the fake binary and invokes the
  restart hook; does not swap while busy, swaps once idle; does nothing when
  the setting is off; logs the unwritable-dir hint exactly once for a
  version; the same fetch error is logged once, not per tick.
- source fallback: primary returns 500 → mirror is used; primary returns a
  verified up-to-date manifest → mirror is not consulted.
- `go test ./...`, `go vet ./...`, `gofmt -l` clean, also with `-tags lark`.

End-to-end (owner, hidden from the implementer): two real binaries built
with a test signing key, the older one run as a detached agent on this Mac,
a locally served signed manifest; within 150 s the file on disk is the newer
build, the pid is unchanged, and the log carries the update line followed by
the newer version's start line.

## Report

Implemented on branch `feat/cli-auto-update` in three commits:

- `9c4a98d` — source fallback. `updateSource()` became `updateSources()`
  returning the release page and the relay's `/dl` mirror in order, with the
  mirror deduplicated when `release_base` already names it. `overSources`
  applies the walk rule for both the manual and the automatic path:
  `ErrUpToDate` ends it, any other error moves to the next base, the last error
  is reported when none work. Manifest fetch + verify + `Select` was split out
  of `downloadSignedUpdate` as `selectSignedUpdate` / `checkSignedUpdate`, so a
  check can learn what is on offer without downloading it.
- `edfdd30` — the feature. New `autoupdate.go` (decision function, loop,
  installer), `autoupdate_unix.go` (`syscall.Exec` in place), and
  `autoupdate_windows.go` (managed: exit 0; detached: respawn like `cmdStart`
  and hand over the pid file). `Agent.Busy()` and `jobStore.runningCount()` in
  `internal/agent`, with a `consoles` counter incremented in `serveConsole`.
  The `auto_update` setting across `internal/config` and `config_cmd.go`.
  `cmdAgent` starts the loop next to `ag.Run`, prints the start line, and
  handles the restart; `printLocalStatus` gained the `自动更新` line.
- `6737a4f` — both environment references, README, both self-hosting guides,
  and `docs/adr/0006-cli-auto-update.md`.

### Tests

New: `autoupdate_test.go` (decision table over off / dev / unversioned / APK /
up-to-date / verify-failure / unwritable / busy / go; loop against a signed
`httptest` server covering install-and-hand-over, wait-while-busy then install
once idle, setting off, the unwritable hint logged once per version, an
identical fetch error logged once and a different one still logged, the loop's
first delay and its stop on hand-over and on a cancelled context; both halves
of the source-fallback rule). `internal/agent/busy_test.go` covers the three
kinds of work and the nil job store. `signedUpdateServer` now takes the
artifact OS, arch and version, so the loop tests download for the platform they
actually run on; its two existing callers pass `linux, amd64, v2.0.0`.
`TestUpdateSourcePrefersReleaseBase` became
`TestUpdateSourcesPreferReleaseBaseThenMirror`.

Commands run on darwin/arm64, Go 1.26:

| Command | Result |
|---|---|
| `gofmt -l .` | no output |
| `go vet ./...` | clean |
| `go test ./...` | all packages ok |
| `go vet -tags lark ./...` | clean |
| `go test -tags lark ./...` | all packages ok |
| `GOOS=linux go vet ./...` | clean |
| `GOOS=windows go vet ./...` | fails, pre-existing (see below) |
| `GOOS=windows go build ./...` | clean |
| `go test -race -run 'AutoUpdate\|Busy\|Decide' . ./internal/agent` | ok |

`GOOS=windows go vet ./...` fails on two test files this change does not
touch, and failed the same way before it:

```
# wanctl/internal/client
vet: internal/client/exec_cancel_test.go:83:16: undefined: syscall.Kill
# wanctl/internal/agent
vet: internal/agent/exec_cancel_test.go:57:21: undefined: syscall.Kill
```

Both files call `syscall.Kill` with no build tag and only a runtime
`runtime.GOOS` skip; they arrived in commit `25f1071` (PR #44). All Windows
production code cross-compiles: `GOOS=windows go build ./...` is clean.

### Deviations from the spec

- **Android starts no loop at all.** The spec's Scope names darwin, linux and
  windows, and justifies the Android exclusion by `runningFromAPK`. That covers
  the packaged app but not a Termux install, which can write its own binary and
  then cannot `exec` it — Android refuses exec of a file in an app's private
  data directory, which is why `selfCommand` invokes the linker explicitly. Such
  an agent would swap successfully and then fail to restart, leaving the device
  with no agent. `startAutoUpdate` therefore returns nil on `GOOS=android`. The
  APK rule stays in `decideAutoUpdate` and in its table, as specified.
- **Two manifest fetches per install.** The decision is a pure function whose
  fetch step returns only a version, so the download in step 7 refetches the
  manifest. The alternative is holding a verified tempfile across the busy gate,
  which parks a half-installed update in a bin directory for up to five minutes
  at a time. Reasoned in the ADR.
- **`decideAutoUpdate` takes injected closures rather than plain values.** The
  order in the spec requires the manifest fetch to happen between step 3 and
  step 5, so a function taking a pre-fetched result could not express "off"
  without fetching. Every dependency is a field on `autoUpdateProbe`; the table
  test supplies stubs and touches no network, filesystem or clock.
- **Nothing else was left out.** Every Behaviour, Files and implementer-side
  Acceptance item is implemented. The owner's end-to-end check with two real
  signed builds was not run — it is stated as hidden from the implementer.
