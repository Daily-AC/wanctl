# 0011 — Cancelling a persistent-session command resets the session

Date: 2026-09-18
Status: accepted

## Context

`wanctl exec --target <dev> "sleep 600"` on the persistent-session path, then
Ctrl-C: the controller exited and the `sleep` on the device ran to completion
(issue #46). PR #44 had fixed the same thing for `--oneshot` and deliberately
left this alone, because a one-shot owns its shell and a session does not: the
session's shell holds the working directory and environment that later
connections rely on.

The issue listed three options. (a) kill the foreground child and keep the
shell, (b) tear the session down and build a fresh one on the next connect,
(c) document that persistent sessions do not cancel. (a) is what a terminal
does and was implemented first. An adversarial review of that implementation
(PR #109, 2026-09-18) found it structurally unfixable, not buggy.

**The agent submits a line, not a process.** `ShellSession` runs a command by
writing it to the shell's stdin, so the shell owns the whole line. Killing the
child it happens to be running now leaves the rest of the line to execute in the
surviving shell. Measured: cancelling
`sleep 600; echo AFTER_CANCEL > f; cd d; export X=changed` while the sleep ran
killed the sleep, and the shell then wrote the file, changed directory and set
the variable — while the caller was told `context.Canceled`. A second `sleep` in
the same line would simply be waited on again. There is no shell-side protocol
to skip the remainder without adding one, and the marker protocol has no channel
to carry it.

**A pid/ppid snapshot cannot prove descent.** The implementation found "the
foreground child" by walking a process table. A parent pid is a number that
outlives its owner: process 100 starts 200 and exits, the new session shell
gets pid 100, and 200 — unrelated, older than the shell — is now in the kill
list. Between taking the snapshot and sending the signal the target can exit and
its pid be reused, so even a correct tree kills the wrong process. This is not a
race window to narrow; process identity is simply not what pid/ppid carries.

**`set -e` destroyed the session anyway.** With errexit set, SIGKILL of the
child made the non-interactive shell exit on the non-zero status without
printing the end-of-command marker. The mechanism whose entire purpose was to
keep the shell lost it, silently, in a common configuration.

**The snapshot was not portable and its failure was invisible.** `ps -A -o
pid,ppid` is not in BusyBox builds without DESKTOP, which OpenWrt disables by
default. The error was discarded, so the cancel did nothing and the caller was
still told the command had been stopped.

A fifth, subtler one: a cancellation goroutine armed by request A could still be
running when A finished and B started, and kill B's child. B reported exit 137
with nothing cancelled.

## Decision

**Option (b): a cancelled session is destroyed, and the next command on that
target builds a fresh one.** The unit of cancellation is the session, because
the session is the thing the operating system can name.

**The shell runs inside an OS process container.** On Unix the session shell is
started with `SysProcAttr.Setpgid`, making it the leader of a new process group
whose id is its own pid; cancelling sends `SIGKILL` to `-pgid`. On Windows the
started shell is assigned to a job object created without
`JOB_OBJECT_LIMIT_BREAKAWAY_OK`; cancelling calls `TerminateJobObject`. Both are
single kernel operations on a set the kernel maintains, so neither needs `ps`,
neither can be defeated by pid reuse, and neither leaves part of the submitted
line running — the shell that would have run it is gone. The job also carries
`JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, so an agent that exits without closing its
sessions still takes them with it.

**A cancellation is bound to the request that armed it.** Disarming takes the
same lock the firing path holds, so it waits for an in-flight cancellation
rather than merely signalling it to stop, and it happens while the request still
holds the session lock. A cancellation therefore either completes within its own
request or does nothing at all; it can never reach the next one. A request whose
context is already cancelled when it takes the session lock runs nothing.

**A kill that fails is reported.** The error reaches the caller attached to the
cancellation, because "the command was cancelled" is the one answer that must
never be returned when the device could not stop anything.

## Consequences

- **Cancelling loses the session's working directory and environment.** That is
  the trade: a cancel that reliably stops everything, for state that does not
  survive it. The `exec` catalog entry and `docs/contract.md` say so, and point
  at `--oneshot` for work you may want to abort and keep nothing from.
- `--oneshot` and `exec_async` are untouched. A one-shot already owns its shell
  and dies with it; an async job is detached from any session on purpose and is
  not cancelled by a controller leaving.
- **What still escapes on Unix:** a process that calls `setsid()` leaves the
  process group and survives, as do daemons that double-fork out of it. That is
  the documented boundary of a process group, and it is the same boundary the
  `--oneshot` cancel hook has had since #37.
- **Nothing escapes the job object on Windows**, because it is created without
  breakaway rights. There is a window between `cmd.Start` and the assignment:
  Go does not expose the thread handle a `CREATE_SUSPENDED` start would need to
  resume, and powershell.exe has not finished loading in that window, let alone
  forked. Nested jobs are supported from Windows 8, so this works inside a
  service host or container.
- A shell builtin that forks nothing is no longer a special case. It used to be
  uncancellable because there was no child to kill; now the shell running it is
  what dies.
- The Windows path is cross-compiled and vetted but has not been run on Windows.
