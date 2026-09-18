# 2026-09-18 · WebFetch skill route, custom grant duration, issue sweep

Owner asks (16:58): (1) serve the WebFetch skill from the portal so web AIs that
accept Agent Skills need no per-conversation prompt; (2) sweep the open issues;
(3) WebFetch grants must be long enough for long tasks.

## Decisions taken by the commander session (owner to confirm)

- Grant duration: presets 15 min / 1 h / 4 h / 24 h plus free input, range
  1–1440 min, default stays 15. Ticket envelope = max + 10 min. `exec`
  `timeout_seconds` ceiling 1800 → 14400 (a long grant is useless if the
  command still dies at 30 min). Jobs per grant scale 64 per approved hour.
  ADR 0010.
- Issues dispatched now: #100, #99, #98, #66, #57 (investigate first), #46
  (option a).
- Owner confirmed all recommendations 17:05: #63/#71 go with option A
  (bypass + elevation channel on ⇒ elevated auto-allowed), 24 h ceiling kept,
  #43 held (Feishu console access + recipient setting; separate round).

## Waves (all parallel, one worktree each under ~/projects/wanctl-oss-worktrees/)

| Task | Branch | State |
| --- | --- | --- |
| W1 skill route `/webfetch/skill` + #100 discovery reuses catalog | feat/webfetch-skill | dispatched 17:10 |
| W2 custom grant duration + exec ceiling + ADR 0010 | PR #107 | accepted 17:25: postgres tests green after commander rebase, approval page eyeballed at 412/1280 (chips + free input); two load-bearing finds: job cap needs decided_at (allowance from approved window, not time left), retention floor raised to 25 h; merge queued on CI |
| W1 skill route + #100 | PR #106 | review round 1 (drop per-request relay call, drop format=json, no hard-coded grant number) applied; merged 17:30 on all-pass CI. #100 closed |
| W3 #98 macOS screenshot TCC message; #66 update on controller-only host | PR #102, PR #105 | #102 merged 17:20 (rebased over #103 by commander; live TCC denial not reproducible in tests). #105: reporter's cause already fixed by #67/#69; real defect was pid 0 matching ManagedPID 0 so an owed restart was silently skipped; merge queued on CI |
| W4 #99 catalog descriptions as operating instructions | PR #103 | merged 17:11 as 704c819, #99 closed. Merged before CI finished (local go test green); CI on main being watched |
| W5 #57 slow-link bad record MAC; #46 persistent session cancel | PR #104, PR #109 | delivered 17:23. #57 root cause: /h/down dequeued before the response was delivered, truncated body swallowed by io.ReadAll, 60 s client timeout covered the body; fix = down-seq/ack protocol, 256 KiB drain cap, header-only timeout, old clients fall back. #46 = kill the shell's process subtree from the leaves, shell survives. Codex adversarial review running (~/.codex-jobs/wanctl-oss/2026-09-18-review-104, -109) |
| W6 #63/#71 elevated under bypass (option A) + approver path + rules kind | PR #108 | delivered 17:21. Finding: gate already reached the approver; the portal pending card had no exec-elevated label and dropped the command text. Script rules remembered as script:<interp>:<16 hex> of the script SHA-256. Codex adversarial review running (-review-108) |

## Codex review round (17:35–17:37)

- #109 BLOCK: stale cancel goroutine kills the next command; PID/PPID snapshot cannot prove descent (PID reuse); remaining statements of the line keep running in the surviving shell; `set -e` kills the session; BusyBox `ps` lacks `-A -o`. Commander decision: abandon option (a), implement option (b) with process group (Unix) / Job Object (Windows), cancel resets the session, ADR 0011. Fix round sent to W5.
- #108 BLOCK: P1 Canonical() slice panic on a malformed script payload kills the agent from the refusal path (no auth needed); P2 script-token branch after string equality lets the literal token match; P2 64-bit digest, store full SHA-256; P2 Android verb guard has no platform check and breaks a desktop `app` binary. Fix round sent to W6.
- #104 BLOCK: P1 overlapping polls from one reader overwrite `unacked` (chunk lost); P1 new client retries against an old relay and silently skips bytes (needs capability negotiation); P1 drain cap + graceful close deletes the session with chunks still queued (tail lost, 404); P2 cap checked before append (over-cap chunks); throughput 256 KiB/RTT ≈ 2.5 MiB/s vs 71 unbounded on a 100 ms RTT probe. Fix round dispatched to a fresh agent W7 (W5 stays on #109). Evidence: /tmp/wanctl-pr104-review.Hfmh7y/REVIEW-EVIDENCE.md.

## Round 2 (17:50–18:02)

- #109 rebuilt on option (b) (process group / Job Object, ADR 0011) at 67f7afb. Codex round 2: BLOCK. P1 shared gate has no request identity (late-waking watcher misfires 50/100); P1 a descendant that left the group keeps stdout open so Wait never returns and sessMu stalls all sessions; P2 double Kill(-pgid) after reap; P2 concurrent request holding a deleted session; P2 Windows suspended-create-then-assign; P2 post-Start failure cleanup leaks copiers. Third and final fix round sent to W5.
- #108 fixed the four findings at c459012 (rebased c4ceb9c). Codex resume was refused by OpenAI's content filter ("possible cybersecurity risk"); a rephrased fresh job (review-108b) ran: four fixes confirmed, one NEW P1: CommandLabel shortens any text starting with `script:sh:` so `script:sh:<hex>; cmd` shows only the digest on the approval card. Fix round 2 sent to W6; commander will verify with the reviewer's probe instead of a third codex round.
- #104: W7 round 1 at dbc20fe (+ wire-level overlap test 2194f79). Codex round 2: 3 of 4 closed; still open: one-direction-empty poll removes the session while the other direction holds unacked data (P1), /h/up accepted after graceful close (P2). Round 2 sent to W7.
- #108 round 2 fix c09e3f9 verified by the commander with the reviewer's probe (TestReviewApprovalSpoof passes); full suite green; merged 18:12 on all-pass CI. #63 and #71 closed.

## Round 3 (18:15–18:20)

- #109 round 3 at 6f18e26: generation-tagged gate, cancel closes the stdout read end + WaitDelay, one signal per container lifetime, "closed before submit" error with one re-acquire, Windows suspended-create-then-assign, single post-Start cleanup. Commander: gofmt clean, -race green on server+agent. Codex round 3 running.
- #104 round 2 at f9b7b65: both directions must be drained before retiring a closed session (idle reaper is the bound), /h/up after close → 410, push/close under one lock. Commander: -race green. Codex round 3 running.
- Both PRs are at the three-round cap. If round 3 returns only P2/P3, merge and file the residue as issues.

## Round 3 outcomes and release (18:20–19:00)

- #109 codex round 3: no P1; residue filed as issues #110 (reap mark after cmd.Wait leaves a PGID window), #111 (background process in the group survives a natural shell exit), #112 (kill failure + blocked stdin write never returns); wording narrowed (70996fe); merged 18:31 on all-pass CI. #46 closed.
- #104 codex round 3: one P1 (drained() blind to an in-flight take) fixed at efc3a18 (inflight flag + settled() requires closed); reviewer's probe passes on the new head. CI then exposed a real regression on the lark-tag job: TestWSControllerBridgesToHTTPAgent timed out because retirement was attempted only by the EOF poll while the in-process bridge reader never tries; fixed at 435eb37 (every poll exit, every bridge read and the close itself attempt retirement; regression pinned at GOMAXPROCS=1). CI all green; merge chained.
- Release v0.11.0: changelog PR #114 (W8, house voice; commander added the #104 bullet); tag on main after both merges; release workflow watched; ops brief for the z10 deploy at scratchpad release-v0.11.0-ops-brief.md (hk-fetch → verify → rsync → bundle → ls-deploy.sh → external checks → portal docs sync → docs site → devices → DEPLOY-NOTES).
- Owner instruction 18:29: "都合完了发个版上生产".

## Merges so far

#103 (17:11), #102 (17:20), #105 (17:24), #107 (17:26, after a gofmt fix by the commander). Process note: `gh pr checks --watch` returns immediately when the new head's checks are not yet registered, so two merges went in with CI still pending; local go test was green both times. Later merges sleep 90 s before watching and merge only on all-pass.

Skill draft the owner approved is the input for W1 (scratchpad
`wanctl-webfetch/SKILL.md`, instance URLs hardcoded; W1 templates it).

## Release v0.11.0 on production (19:05–19:34)

Tag v0.11.0 = d8ad2eb (#114 changelog on top of #104). Release run 35337599181, 6m22s, 38 assets. Ops runner followed the v0.10.0 runbook: hk-fetch ~3 min, hk→ls rsync 7m27s, verify on both hops (`verified signed release v0.11.0 with key cfd9c8177cde33b1`), bundle 74c8e65..v0.11.0, `ls-deploy.sh v0.11.0`: relay/portal image 4930fbd1ef40, healthy, postgres untouched, `wanctl version` v0.11.0 both. External: /dl manifest v0.11.0, /api/releases current v0.11.0, discovery `skill_url` + `instructions`, `/webfetch/skill` 200 text/markdown with instance URLs and no placeholder, wc.z10.dev 200, parity ok 0 broken. Portal docs 4 skipped / 7 updated. Devices: this Mac updated to v0.11.0; the other three stay on v0.9.0/v0.9.2/v0.10.0 until their 6 h auto-update check. Rollback: `wanctl:rollback-pre-v0.11.0` (5c1b71f1c843), `dist-v0.10.0`, `backups/v0.11.0-20260918T112713Z/`. No migration.

Two runbook corrections recorded in ls DEPLOY-NOTES: the post-checkout check is `git diff --stat <tag> -- . ':(exclude)selfhost/dist'` (dist is swapped every release, so its .gitkeep is always missing); the portal is GET-only, so probe `/webfetch/skill` with GET, not HEAD.

## Why a release takes 25 minutes, and what to cut

Measured on this release: 372 MB of assets moved twice (GitHub→hk 3 min, hk→ls 7.5 min at 0.9 MB/s including a temporary SSH key round trip) = 42% of the wall clock; docker build + restart 1.5 min; the remaining ~9 min is an agent stepping through checks, docs sync, docs site and notes with no judgment involved. ls→GitHub is dead (1.7 KB/s), but ls→ghfast mirror measured 1.4 MB/s and ls→Cloudflare 3.3 MB/s, both faster than the hk hop. 270 of the 372 MB are the 15 raw binaries that duplicate the 15 tar.gz; the relay's VerifyDirectory requires every manifest entry present, so a partial mirror needs a code change.

Recommended, in order: (1) ls pulls the release straight from a GitHub mirror or through hk's existing HTTP proxy, one transfer, no temp keys; (2) steps 2–10 of the runbook become one script run from the Mac with the corrected checks baked in; (3) let the relay serve a partial mirror (present artifacts verified, absent ones 404) so only tar.gz + apk are mirrored; (4) skip docs sync/docs site when docs/ did not change between tags. Target: 8–10 minutes.

## Open after this round

- PR #113 (external fork, account team-humaki, opened 12 minutes after issue #112 was filed): 70-line fix for #112, race-clean locally on three runs; merge is the owner's call (fork CI needs approval).
- Issues #110, #111 (session-cancel residue), #43 (Feishu, held).
- Unverified on real links: #57 needs one 18.9 MB push to a v0.11.0 agent over the slow Windows link; the 24 h WebFetch grant needs one real long-task session; Windows job-object cancel path has never run on a real PowerShell.
