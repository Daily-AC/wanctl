# Run commands as a logged-on Windows user

`wanctl exec --as <username>` lets a Windows agent running as SYSTEM start a
fresh shell with a logged-on user's token. It is de-elevation, not a request
for administrator privileges. Ordinary `exec` policy and session capabilities
still apply, and the requested account is recorded as `as` in the event log.

```sh
wanctl exec --target home-pc --as alice 'whoami; $env:USERPROFILE; $env:LOCALAPPDATA'
wanctl exec --target home-pc --as 'DESKTOP\alice' --script ./install.ps1
wanctl exec --target home-pc --as alice --cwd 'C:\Users\alice\My Project' 'git status'
```

The same option is available as `as` on `wanctl_exec` in MCP. Both the controller
and device agent must be updated. An older agent rejects the new `exec_as`
request before running anything; the controller does not retry it as ordinary exec.

- The named account must have exactly one active or disconnected interactive
  logon. A locked desktop is fine. No password is needed. No matching session,
  multiple matching sessions, and a non-SYSTEM agent are errors.
- Account names are resolved to SIDs, including `DOMAIN\user`, `MACHINE\user`,
  and `.\user`. The agent uses `WTSQueryUserToken` and duplicates that primary
  token. It never requests a linked elevated UAC token.
- `CreateEnvironmentBlock` supplies the user's environment, including
  `USERPROFILE`, `LOCALAPPDATA`, `TEMP` and PATH, without inheriting SYSTEM's
  environment. The user's registry hive must already be loaded by the logon,
  so HKCU belongs to the user. The default working directory is their profile.
- Go's token-aware process creation calls `CreateProcessAsUserW`. The private
  token copy is assigned to the agent's Windows session so the existing output
  pipes can be inherited. This is a noninteractive command, not a GUI launched
  on the user's desktop; the user's original token is unchanged. Windows
  [does not allow handle inheritance across sessions](https://learn.microsoft.com/en-us/windows/win32/api/processthreadsapi/nf-processthreadsapi-createprocessasuserw).
- `--as` always starts a fresh shell, preserving output streaming, exit codes,
  explicit `--cwd` and cancellation. It cannot be combined with `--elevate`,
  `--via`, or workspace execution. Linux, macOS and Android reject it.
- `--script` uses the existing encoded transport, so it does not stage a script
  in `C:\Windows\Temp`. For scripts exceeding the inline size limit, create the
  file through an `--as` command in a user-accessible directory before running it.
  Native push/pull/read/edit/write operations have no `--as` option in this change.

## Windows verification

Linux tests and a Windows cross-build do not exercise Windows token APIs. On
a Windows machine with the target user logged on, run this from a SYSTEM shell:

```powershell
$env:WANCTL_TEST_AS_USER = 'DESKTOP\alice'
go test ./internal/server -run 'TestRunAs' -v
```

The opt-in test runs an encoded PowerShell script and checks the child SID,
profile and working directory, user temp environment, UTF-8 output, file owner,
HKCU writes in the user's SID hive, a second user process updating/deleting the
file, exit codes, unknown users, and cancellation before process creation.
Without the environment variable, the token execution tests skip. A normal
non-SYSTEM test run checks that the feature refuses to run.

Then verify through the deployed agent with the CLI commands above, including
an actual per-user installer, `--as` while the user is signed out, and Ctrl-C
during a command with a long-running child. These checks require the Windows
machine and are not covered by cross-compilation.
