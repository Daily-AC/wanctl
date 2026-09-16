# WebFetch acceptance — 2026-09-16

The implementation was exercised through Qwen3.8-Max in its normal web chat UI,
using a public HTTPS WebFetch endpoint and an isolated wanctl deployment. The
owner portal was loopback-only with the test fixture's fixed identity; production
GitHub OAuth was not reconfigured or used for this run. The relay used real
PostgreSQL, and the device used wanctl mutual TLS and normal device policy.

Observed sequence:

1. Qwen read the entry page, followed its start URL, received `pending` and
   returned the owner approval link. No device operation ran.
2. The owner portal selected the single isolated device and approved 15 minutes.
   The displayed device fingerprint matched the locally generated device identity;
   the controller fingerprint was independently derived from the local adapter seed.
3. Qwen's first command was refused because the controller was not paired.
   The existing wanctl pairing page then trusted the verified controller.
4. Qwen retried with a new request ID, and `printf wanctl-webfetch-ok` returned
   the expected stdout and exit code 0.
5. Qwen wrote and read a text file containing Chinese, newlines, and `& + % # ?`.
   Both task results and the actual device file were **62 UTF-8 bytes**, SHA-256
   `61476ad9436a5bc24c4060a5f7ae79177d700829ca2cdcf6900c757417c7acb6`.
6. The owner revoked the delegation through the existing Access tokens page.
   Qwen's attempt to read the previous result and submit a new command both
   returned HTTP 403 with status `revoked`. Independent HTTP checks agreed.
   The ledger retained four jobs (one unpaired refusal and three successes),
   with no post-revocation job. The device log recorded only the three authorized
   operations, each linked to its authenticated grant, credential and session.

The device was never put into bypass mode. Its existing rules allowed only the
test directory and the exact harmless command. No credentials or live browser
tickets are included in this record.

An initial browser attempt encountered a disconnected development SSH tunnel
and correctly reported 502. After restoring the test ingress and adding reconnect
supervision, the complete workflow above passed. That tunnel is a development
fixture, not part of the WebFetch protocol.

Automated validation includes real-PostgreSQL lifecycle/concurrency/retention
tests, both transports and all four carrier combinations, device-scope and
management rejection, credential-bound sessions, active revocation and expiry,
late human approval after revocation, old-agent and upstream compatibility,
bounded client cancellation, immutable request deduplication, and scoped audit.

This establishes the observed Qwen workflow and tested enforcement paths. It is
not a claim that every web AI can fetch arbitrary URLs or that arbitrary
background side effects can be rolled back. See [WebFetch](webfetch.md) for
limits, compatibility, deployment and rollback requirements.

## v0.9.1 contract follow-up — 2026-09-17

User tests exposed two contract gaps: clients were guessing whether `target`
meant a namespace, an ID or their combination, and a public entry response
carried a live ticket that a fetch-provider cache could reuse. The revised
discovery document is static; independent nonce URLs create requests, and the
authenticated portal can generate a fresh complete connection prompt.

The revised contract was tested through a public HTTPS endpoint backed by the
same isolated normal-policy fixture and real PostgreSQL:

- DeepSeek read the full continuation URL, discovered the exact `devices[].target`,
  constructed the GET call, polled its result and correctly stopped for pairing.
  After owner pairing, a new rid produced `done`, exit code 0 and stdout
  `wanctl-webfetch-ok`. The database independently recorded the unpaired refusal
  and the one successful execution. After owner revocation, DeepSeek read the
  HTML error envelope and reported `status: error`, `http_status: 403` and
  `error_code: access_denied`; no post-revocation job was created.
- Complete URLs were supplied in continuation messages. An initial DeepSeek
  attempt from the generic discovery URL generated a nonce but did not fetch
  its constructed URL until that complete URL was explicitly supplied. This
  is not an unattended one-prompt onboarding claim.
- Gemini's web UI created a distinct real request and read its approved
  manifest. Its first constructed call reported `PERMISSION_DENIED` without a
  job. Supplying the same complete call URL explicitly in a later message
  created a real job; the ledger recorded the expected unpaired refusal.
  This run does not establish a complete Gemini execution flow or the exact
  cause of the earlier client-side denial.
- A Chrome ChatGPT test with Web search enabled returned a platform
  "Unusual activity" error before an interface result. ChatGPT compatibility
  was not established by that attempt, and the block was not bypassed.
- Two clicks on the portal copy action produced different 48-hex nonce URLs;
  the page and copy action did not create or approve any grant.

Regression tests cover credential-free cached discovery, independent requests,
unfilled templates, discovery-driven execution, readable HTML denials, strict
JSON HTTP errors, unchanged revocation/scope/idempotency enforcement, and
owner-safe recovery pages. The full Go race suite with real PostgreSQL passed;
the bilingual documentation build passed with no broken links.

These observations concern actual requests and ledger outcomes, not a model's
self-description of its tools. They do not promise that every web search
interface can fetch arbitrary dynamically constructed URLs.
