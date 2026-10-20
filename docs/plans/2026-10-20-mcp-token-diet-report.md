# MCP token cost — 2026-10-20

## Why

A user reported that driving wanctl through MCP costs too many tokens. Every
number below comes from `count_tokens` (claude-opus-5-5) over what the real
stdio server sent, against a real device on the production relay.

## Where the tokens went

| Source | v0.24.0 | Cause |
| --- | --- | --- |
| Tool definitions | 18,401 (stdio, 23 tools); 18,004 (hosted, 22) | The same rules restated in every tool: pairing URLs, first-contact pins, the AGENTS.md rule, workspace mode, Android adb troubleshooting inside `elevate`. Paid on every request by hosts that load all tools. |
| `initialize` instructions | 971 | Re-listed every tool the host already had from `tools/list`. |
| Result shape | +20–25% on file content; ~235 tokens per workspace command; 2x on hosts that show both forms | Claude Code and Codex (openai/codex#2594) give the model `structuredContent`, not the text, so every result arrived as escaped JSON. |
| Page sizes | up to ~70k tokens per `wanctl_read`; workspace output paged 48 KiB at a time until all retained output was read | 256 KiB read cap; "continue while next_offset < retained_bytes". |
| Round trips | every command over 250 ms in a workspace, every poll | exec waited 250 ms; polls returned at once. Each extra call is a full model turn. |

## What changed

- Results are text only. Output schemas, `structuredContent` and their
  validation are gone; workspace results are a few `key: value` lines and the
  output.
- Each rule is said once, where the model needs it: in the description when it
  decides which tool or argument to use; in the refusal text when it is about an
  error (adb remedies are appended from the catalog when that error arrives);
  once in the instructions, which no longer list the tools.
- `wanctl_id`, `wanctl_trust`, `wanctl_rules` and `wanctl_server_logs` left MCP
  (owner decision, 2026-10-20). The CLI keeps them. `wanctl_status` shows the
  fingerprint on the hosted endpoint too. `wanctl_push`/`wanctl_pull` are not
  offered on the hosted endpoint, where they could only refuse.
- `wanctl_read` returns at most 48 KiB per call over MCP. Workspace output over
  48 KiB is its first 8 KiB and last 40 KiB around the gap's offset.
- Workspace exec and both polls wait up to 20 s for the command to finish.

## Result

| | v0.24.0 | v0.25.0 |
| --- | --- | --- |
| Tool definitions, stdio | 18,401 | 8,457 |
| Tool definitions, hosted | 18,004 | 7,714 |
| Instructions | 971 | ~460 |

Dev-loop scenario (peers, workspace enter, git status/log, a 160-line and a
1,900-line read, a 3-second command, `git log --stat -60`, read/edit/write, a
5-second background job, exit), device agent v0.24.0 both times:

| | v0.24.0 | v0.25.0 |
| --- | --- | --- |
| Tool calls | 19 | 15 |
| Result tokens, Claude Code / Codex | 65,585 | 43,907 |
| Result tokens, host showing both forms | 123,731 | 43,907 |

Part of the read saving is content not delivered by default: a model that wants
the rest of the 1,900-line file makes a second call.

## Decisions taken by the owner

- Drop structured output entirely rather than slim it. The model gets the same
  information from the text; the cost is that ChatGPT's developer mode shows its
  "missing output schema" hint on the eight tools again, and PR #119's contract
  is withdrawn. No format parameter: a later program that needs typed data can
  bring it back.
- Remove the four tools above from MCP.

## Not changed, candidates for later

- The 48 KiB output page. Claude Code's own Bash keeps 30,000 characters and
  Codex about 10 KiB; a smaller page would save more on verbose builds at the
  price of more follow-up greps. Worth deciding from real sessions rather than
  guessing.
- The CLI skill (`internal/relay/skill.md`, ~4.8k tokens when invoked) has the
  same kind of repetition. It is loaded only on demand.
- OpenCU results (`wanctl_act` tool calls) pass through unchanged; their size is
  OpenCU's to tune.
