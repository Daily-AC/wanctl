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
