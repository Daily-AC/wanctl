# 2026-09-17 · wanctl as a thin harness: read/edit tools, CLI help as thin MCP, WebFetch for strong models

Owner decisions (Feishu thread with 耀宇 + this session, 2026-09-17 19:30):

1. Add native `read` and `edit` file tools on every surface: agent protocol, MCP (stdio + hosted), CLI, WebFetch. `exec` stays. Motivation: 学弟's mcpx comparison (paged read, hash-checked edit instead of cat/sed via exec) and lower odds of web-AI safety blocks ("couldn't determine the safety status") than multi-line shell scripts.
2. CLI help is rewritten as a "thin MCP": one command catalog is the single source for MCP tool descriptions and `wanctl help <cmd>`; `wanctl` alone prints a short index.
3. WebFetch stops accommodating weak models (DeepSeek web). Target strong models (Qwen 3.8-Max, ChatGPT). Entry is ONE user prompt; the model guides the user through device authorization → device fingerprint pairing → exec/read/edit. Long jobs (Blender-length, ~10 min) must succeed through next_url polling; today MaxRequestTime=60s killed 耀宇's run with "context deadline exceeded".

## Waves

- Wave 1 (parallel): A core read/edit (agent+MCP+CLI); C1 WebFetch strong-model flow + long jobs.
- Wave 2 (after A merges): B help catalog (thin MCP); C2 WebFetch read offset/limit + edit tool over the new agent op.
- Acceptance on real devices by the commander session (zyl Windows, this Mac, ls) before any production release. Production release approved by the owner 19:48 (耀宇 and his 学弟 are the testers); ship after commander acceptance.

## Status

| Task | Branch / PR | State |
| --- | --- | --- |
| A core read/edit | PR #91 merged 20:33 as 5e74900 (fix round: OOM cap, file events, UTF-8 range, line-boundary cut) | real-device acceptance after release |
| C1 WebFetch strong-model flow + long jobs | PR #92 merged 21:00 · ADR 0009 | three review rounds (Codex): F2–F5 fixed, F1 dropped by owner decision, busy-before-dedupe P1 fixed; cut candidate left in: owner_start_url bootstrap path |
| B help catalog + contract doc | PR #93 merged 21:2x: internal/catalog single source, docs/contract.md + drift test, harness framing, AGENTS.md rule; follow-up: rewrite the 17 inherited descriptions as instructions |
| C2 WebFetch read/edit | PR #94 merged 22:0x: read_text offset/limit, edit_text, file_refused, all=*bool; edit UnsupportedError → unknown until client fix lands (wave 3) |

## Product definition discussion (20:00)

Owner shared a ChatGPT critique ("wanctl is a secure remote shell, not an agent workspace") and asked whether to build a harness SDK. Commander position: keep the trust layer, expose a minimal primitive set (exec, exec_async/poll, read, write, edit, screenshot; push/pull for binary) identically on CLI/MCP/WebFetch; no SDK until an embedder exists — the command catalog doubles as the contract doc. Awaiting owner decision on (1) primitive list, (2) SDK deferral.

## Review round 1 (20:20)

- PR #91 (Opus review): MERGE WITH FIXES — edit all=true can OOM the agent (projected size unchecked, fileops.go:249), rejection events for the two kinds not logged as file events (delegation.go:40), read range not validated as UTF-8 beyond the 8 KiB sniff, truncation cuts mid-line so paging cannot resume. Fix round sent to the implementing agent.
- PR #92 (Codex cross-vendor review, job ~/.codex-jobs/wanctl-oss/2026-09-17-review-webfetch-strong): BLOCK — F1 no owner-level slot budget (one user × 16 grants can hold all 64 slots), F2 implicit timeout default in the rid hash breaks same-URL replay across deploy, F3 approved JSON lost the same-rid retry rule, F4 advertised deadline ignores grant expiry, F5 unknown text claims deadline elapsed on early interruption. Fix round sent. Inherited (not fixed): exit code lost when FinishJob fails; shell daemonization outlives grants.

## Owner update (20:34)

- Relay deployment will move off the hk relay to the owner's own 5090 box behind a Cloudflare Tunnel (China-first users; IP 优选/回源 notes at https://cf-optip.tagzxia.com/kb/). Consequence: single-owner relay, so PR #92 review finding F1 (cross-owner slot starvation) is dropped; F2–F5 remain.
- PR #91 merged (5e74900). Wave 2 started: help catalog (feat/help-catalog).

## Product definition (owner, 20:43)

"wanctl 是网页 AI 的外部 harness，两者组成一个 agent。" Not an SDK. Consequences: the tool catalog is the agent's system prompt; README / portal AI 接入 / contract intro adopt this framing; harness gaps to close: desktop screenshot (eyes), text write, a "read AGENTS.md first" operating rule (instruction only, no feature).

## Wave 3 (dispatched 21:25, branch feat/harness-pi-parity)

Owner approved (21:05): primitives = exec, exec_async/poll, read, write, edit, screenshot + push/pull; SDK replaced by the "external harness" framing; "borrow from pi". Five items: text write; edit takes edits[] (fewer approval prompts in web AIs); desktop screenshot as MCP image content; exec truncation keeps the tail and spills full output to a device file; MCP initialize `instructions` built from the catalog (= the harness system prompt), also `wanctl help --instructions`. Release once after wave 3 unless it slips past ~23:00.

## Review round on PR #94 (21:45, Codex job ~/.codex-jobs/wanctl-oss/2026-09-17-review-webfetch-read-edit)

BLOCK. P1: internal/client/fileops.go:144 turns a post-send EOF into UnsupportedError, so WebFetch reports "device too old, nothing ran" after an edit that actually committed → double-edit path. Client-side fix assigned to the wave-3 agent (ResultLostError); WebFetch-side safe mapping assigned to the #94 agent. P2: explicit all=false not in the rid hash. Plus wording narrowing (target not replaced ≠ nothing written) and three missing tests.

## Wave 3 review (22:25, Opus)

PR #95 MERGE WITH FIXES: batch-edit allocates before the size check; no cap on edits (CPU DoS) → cap 64; spill write errors discarded → latch + no path; spill size/count caps (8 MiB / 32 files); read lost-result text self-contradictory; spill path dropped on error frames; truncation line blames agent version when spill merely failed; SpillBytes unused; non-EOF post-send errors not ResultLost; array param rendering hardcoded. Owner-level decision kept: desktop screenshot auto-approved under bypass (called out in PR body + description). Fix round sent.

## Release v0.10.0 (22:45)

PR #95 merged 74c8e65 (wave 3 + client ResultLostError); changelog #97; flake fix #96 (closes #90). Tag v0.10.0 = 74c8e65, release workflow run 35235648880. Deployment dispatched to an ops agent following the v0.9.x runbook (hk-fetch → rsync → verify → bundle → ls-deploy.sh → verify → portal docs sync → docs site → device auto-update). Commander acceptance on real devices follows (scratchpad acceptance-hidden.md).

## Release outcome and acceptance (23:50)

- v0.10.0 on production: relay/portal image 5c1b71f1c843, manifest v0.10.0, MCP initialize carries instructions (39 lines), 21 tools, WebFetch discovery has the two checkpoints and 10 security keys. Portal docs synced (7 updated), docs site redeployed (0 broken links). ls clone had 45 root-owned paths from the 09-16 hotfix; chowned to ubuntu before checkout. Rollback: `wanctl:rollback-pre-v0.10.0`, `dist-v0.9.4`, `backups/v0.10.0-20260917T151006Z/`.
- Devices: 宋喆云服务器 auto-updated; Mac updated by hand (v0.10.0); 5090 (v0.9.0, agent runs under the system profile, exec has no `wanctl` on PATH) and 光影精灵 (v0.9.2, `wanctl update` via exec hits rename Access is denied) wait for the agent's own 6-hourly check.
- Commander acceptance on the Mac device, all PASS: old-agent read/edit → update message in 9–12 s; CRLF preserved through edit; ambiguous old refused; --all; mode 0755 kept; expected_sha256 race refused with the new sha; unicode path; write creates parents (0644) and keeps 0755 on overwrite; exec tail via MCP shows last 49152 of 298890 bytes with the spill path, spill holds all 6000 lines (0600); multi-edit and overlap refusal via MCP; file events logged as READ/WRITE/EDIT with paths.
- FAIL on the Mac: `wanctl screenshot` → `screencapture failed: could not create image from display` — macOS Screen Recording (TCC) not granted to the agent process; needs the owner to grant it in System Settings (issue #98).
- Not run tonight: Windows acceptance (CRLF/unicode path/read-only attr) until those agents update; WebFetch one-prompt flow with a real web AI (needs a human at the approval steps) — hand to 耀宇/学弟.
- Follow-up issues: #98 (macOS TCC guidance), #99 (rewrite 17 inherited descriptions; REFUSALS header wording), #100 (WebFetch discovery reuses `help --instructions`).
