# Completed ChatGPT web workspace trial, 2026-09-22

## Result

The real ChatGPT web development trial completed through the **wanctl Workspace
Preview** connector. One chat implemented and tested a Python program; a second
chat exercised a separate workspace under the same OAuth account. Environment,
cwd, file operations, result recovery, command deduplication and explicit exit
were exercised against the real Linux test agent.

This completes the web trial that was initially blocked in the
[CLI/web acceptance record](2026-09-22-cli-web-acceptance.md). The feature remains
on the development branch and preview endpoint; it is not a public release or
an upgrade of the normal hosted `/mcp` endpoint.

## Authorization and scope

The owner approved the exact first-contact fingerprint, then explicitly allowed
the preview plugin within the acceptance conversations. The browser's **Allow
for this conversation** option was selected separately in the two test chats.
All task work was directed at the isolated test device's `web-project` and
`web-other` projects. No operator connection was used to do the web task.

Permission settings were read back after the trial:

- Global default: allow low-risk actions, unchanged.
- Normal wanctl connector: allow all actions, unchanged.
- Preview connector default: ask before writes, unchanged.
- The owner-authorized overrides applied only to the acceptance conversations.

The earlier suspicious-instruction warning was fixed in the
[trust approval change](2026-09-22-trust-approval-diagnosis.md). The underlying
device identity, pairing and policy checks remained in place.

## Actual development task

Chat A read the project instructions, requirements and sample CSV through
`wanctl_read`. It created `report.py` and `test_report.py` with `wanctl_write`,
then ran the program and ten unittest tests through workspace exec.

The program uses Python's standard library and Decimal arithmetic, validates
quantity and price before filtering, supports exact Chinese-name selection,
and emits quantity and two-decimal amount as JSON. The actual sample returned
quantity **6**, amount **4.00**; selecting 苹果 returned **3**, **0.40**.

Review caught a scope issue in the generated tests: NamedTemporaryFile initially
used the system temporary directory, with unittest cleanup registered. Chat A
read the file, applied a SHA-checked `wanctl_edit` to place temporary CSV files
inside the project instead, and reran all ten tests successfully. Workspace
path resolution is not an OS filesystem sandbox; a program's temporary-file
location still needs to be chosen correctly.

The written sources and the subsequent patch were reconstructed locally and
matched against the device-returned SHA-256 values. They are retained with the
private acceptance artifacts.

## State, recovery and lifecycle evidence

| Probe | Observed result |
|---|---|
| Independent entries in two chats | Different workspace IDs and project roots |
| Chat B sets its environment and changes directory | Later call returns `web-chat-b` and `web-other/session-b` |
| Chat A works after B changes its state | A still returns `web-chat-a` and its own project cwd |
| File read after B's shell changes directory | Relative `AGENTS.md` still reads from B's project root |
| Async execution | Initial response reports `running`, `done=false` |
| Reload the ChatGPT page and continue | Polling the original request returns all test output and exit 0 |
| Repeat the exact async tool arguments and request ID | Same completed result; marker file remains one byte `x` |
| Close A, then operate B | A reports `closed`; B still has its own environment and cwd |
| Close B | B also reports `closed` |
| Read with the old closed references | Both chats report the expected workspace-unavailable refusal, without reopening or falling back |

The downloaded trace verifies byte-equivalent replay arguments, identical
completed results and the one-byte marker's hash. It does not establish that
the 45-second job was still running at the exact instant of page reload, nor
does page reload measure ChatGPT's MCP protocol-session headers. The previous
local Codex trial separately exercised an actual network disconnection while a
remote command was running.

The exports contain 25 successful tool results. The two expected failed reads
after close are visible in the chats' reported results but absent from those
downloaded tool-call lists; they are retained as visible-page evidence, not
misrepresented as exported raw error receipts. The saved result count is not a
claim to capture every request attempted during the transient web error.

## Friction and completion

Chat A paused after requirement reads. A follow-up encountered the page error
`Unusual activity has been detected from your device. Try again later.` One
delayed retry on the same page resumed the task and completed implementation.
This was real friction: the trial demonstrates a working remote workflow,
not a promise that every web session will be uninterrupted.

Both workspace shells were explicitly closed. The preview service, sample
project files and review conversations remain available for owner inspection.
Local ignored evidence is in `artifacts/web-acceptance-20260922/`, including
the ten raw exports, final visible states, exact project sources and
`assessment.json`. Private device and conversation identifiers stay there.

Native host-tool replacement, interactive PTYs, a persistent Python interpreter
and cross-controller shell takeover remain outside this implementation. CLI
and local stdio with one controller identity can already hand off a workspace;
hosted OAuth uses a separate controller identity.
