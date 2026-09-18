---
name: wanctl-webfetch
description: Operate the user's own computers and phones through wanctl using only your URL-reading (web fetch) tool. Run a shell command, read a text file, patch a file in place, or write a new file on a named device. Use whenever the user asks to do something ON one of their devices — "在 home 上跑一下", "看看我 Mac 上那个文件", "帮我在那台 Windows 上装…", "把这段改到服务器的配置里", "run this on my PC", "check the log on the lab box" — or mentions wanctl. No MCP, no code execution and no login on your side; the user approves access in their browser.
---

# wanctl over WebFetch

Everything is an HTTP GET with your URL-reading tool. The protocol is
self-describing: every response tells you the next URL. You lead; the human
acts exactly twice (approve access, approve pairing).

- Protocol entry: `@WANCTL_RELAY@/webfetch/v1`
- Quick reference: `@WANCTL_PORTAL@/webfetch/help`

Add `?format=json` (or `&format=json`) to any protocol URL when your fetch tool
mangles HTML.

## 1. Get access (once per conversation)

1. GET the protocol entry and follow its `procedure`.
2. Make a fresh `client_nonce`: 48 lowercase hex characters from 24 random
   bytes. Put it into `start_url_template` and GET that complete URL. Never
   fetch the literal template.
   - If you have no way to produce real randomness, do not invent one. Tell
     the user to open `@WANCTL_PORTAL@/webfetch/connect` and paste back
     the "connection prompt" from that page; it already contains the start URL.
3. The response echoes `client_nonce`. If it differs from yours you read a
   cached page: start over with a new nonce.
4. Show the user `approval_url` and `continuation_prompt`. Ask them to pick the
   devices and a duration longer than the task needs, and approve. Then stop
   and wait. Never open the approval link yourself; fetching it approves nothing.
5. When the user says they approved, GET `status_url` until `status` is
   `approved`. `rejected` or expired: say so and stop.
6. The approved manifest gives `devices[].target`, `call_endpoint` and
   `tools[].call_url_template`. Copy a `target` exactly as given
   (`namespace/device_id`); never guess or abbreviate it.

Grants last at most 60 minutes and are per conversation. Keep the full
`status_url` and the `exec` call template in your reply so later turns can
continue without redoing step 1. If a later call says the grant expired or was
revoked, start again from step 1.

## 2. Call a tool

```
GET CALL_ENDPOINT?rid={rid}&tool={tool}&target={target}&...
```

URL-encode every value once; the slash in `target` becomes `%2F`.
`rid` is your operation id, 1–64 letters, digits, `-` or `_`. One rid means one
operation.

| tool | required besides rid and target | notes |
| --- | --- | --- |
| `exec` | `command`; optional `cwd`, `timeout_seconds` (1–1800, default 300) | one-shot shell; stdout/stderr capped at 16 KiB each |
| `read_text` | `path`; optional `offset` (1-based line), `limit` (lines) | returns lines, `total_lines`, whole-file `sha256`; page on with `offset=last_line+1` |
| `edit_text` | `path`, `old`, `new`; optional `all`, `expected_sha256` | replaces one exact span in place, rest of file untouched |
| `write_text` | `path`, `content` (max 2 KiB) | overwrites the whole file |

Pick the right tool:

- Look at a file with `read_text`, not `exec` + `cat`.
- Change an existing file with `edit_text`, not `write_text`. Pass the `sha256`
  from your read as `expected_sha256` so a concurrent change is not lost.
  One edit per span; `old` and `new` travel in the URL (8 KiB total).
- Anything with quotes, `$`, pipes or several statements: prefer writing a
  script with `write_text` to a temp path, then `exec` it. On Windows the device
  shell is PowerShell.

## 3. Read the result

The call returns `job_id` and `result_url`. GET `result_url`; while it is
`running`, follow `next_url` after `poll_after_seconds`. A build or install
legitimately runs for minutes, up to `deadline_at`.

- `done`: report the real output. For `exec` that is `stdout`, `stderr` and
  `exit_code`.
- `failed`: an error. Read `error_code` and `result`.
- `unknown`: the operation may have run. Never repeat it. Ask the user to check
  the device.
- A page that loads is not a result. Always check `status` and `http_status`.

## 4. Pairing (first command on a device)

If a result has `error_code: "pairing_required"`, nothing ran
(`execution_started: false`). Show the user `pairing_url`, wait until they say
it is approved, then submit the same operation under a NEW rid. Never open the
pairing link yourself.

## Rules for rid and retries

- Lost response or fetch cut off midway: GET the identical URL again with the
  same rid and arguments. That returns the recorded job and cannot run it
  twice. It is the stored result, so re-read a file under a new rid when you
  need its current content.
- New rid only after a result that says nothing ran: `pairing_required`,
  `adapter_busy` or `file_refused`, each with `execution_started: false`.
  Never after a plain `failed`, never after `unknown`.
- A denied command (device policy) is not a tool bug. Tell the user the device
  must allow it; do not retry blindly.

## Never

- Simulate a response, invent a URL, or claim a command ran without a `done`.
- Reuse a nonce, approval, status or call URL from another conversation, or
  search for one.
- Publish a transcript containing status or call URLs; they are bearer
  credentials until the grant expires.
