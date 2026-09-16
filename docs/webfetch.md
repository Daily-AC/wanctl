# WebFetch access

WebFetch lets a web AI that can read URLs use wanctl without an MCP connector.
It is an optional controller adapter hosted alongside the relay. The owner
approves a short-lived delegation in the existing wanctl portal; device trust,
rules, mode and per-request approvals still decide what runs.

The same protocol serves URL readers, Python/JavaScript HTTP clients and future
SDK or MCP adapters. No permission logic depends on the AI provider or the
client's implementation language. A client must be able to fetch a specified
HTTPS URL and read its response; searching an index alone is not sufficient.

## Owner workflow

For a web chat, start in the authenticated portal at **Settings → Connect web
AI** (`/webfetch/connect`). Each copy generates a fresh connection prompt with a
complete URL using cryptographic randomness; the AI need not invent a nonce or
remember a hidden tool result. Opening or copying the page grants no access.
Use a new prompt for every conversation. SDK clients can perform discovery and
generate their own secure random nonce as follows.

1. Ask the AI to open `https://RELAY/webfetch/v1`. This static discovery page
   contains no ticket. The client generates a fresh `client_nonce` (24 cryptographically random
   bytes encoded as 48 lowercase hex characters), substitutes it into
   `start_url_template`, and GETs that unique URL. Verify the returned
   `client_nonce` matches: a mismatch means the fetcher served another request.
2. The AI returns an `approval_url` and `continuation_prompt`. Open the approval
   link yourself, sign in to wanctl,
   verify the controller and selected device identities, choose your devices
   and a duration, and approve. Fetching this URL cannot approve a request.
3. Send `continuation_prompt` back to the AI after approval. It includes the
   complete `status_url`: some web chats do not retain previous tool results or
   enable URL reading when a later message only says "approved". The status
   document contains exact `devices[].target` values, tool input schemas and
   URL templates. The AI must copy a target rather than infer its format.
4. On first use, the selected device may require ordinary controller pairing.
   The AI must show that owner link, not approve it. Pairing does not change
   the device's operation rules or enable bypass.
5. Revoke the delegation from **Settings → Access tokens** when finished.

Only owned devices with persistent IDs and recorded fingerprints can be selected
in this initial version. Ordinary cross-account sharing is unchanged. Device
renames do not change grants; device removal or certificate rotation invalidates
them. A grant has device-use rights only, never console/management rights.

The device's mode is authoritative: granting use of a bypass-mode device gives
the client broad use of that device. WebFetch does not pretend that an exec
permission can be separated from what an arbitrary shell command can do.

## Operator setup

Upgrade **both the relay and the controlled agents**. Older agents do not
advertise delegated-session enforcement, so delegated dials fail closed.
The existing portal must also be upgraded for the approval page.

WebFetch is disabled unless `WANCTL_WEBFETCH_SEED` is set. Configure:

| Variable | Meaning |
| --- | --- |
| `DATABASE_URL` | Existing wanctl PostgreSQL database; required for durable grants and request deduplication |
| `WANCTL_WEBFETCH_SEED` | Secret hex seed, at least 32 decoded bytes; keep it in the operator's secret store |
| `WANCTL_PUBLIC_ORIGIN` | Canonical public HTTPS relay origin used in AI-facing links |
| `WANCTL_WEBFETCH_PORTAL_ORIGIN` | Canonical public HTTPS portal origin used in owner approval links |
| `WANCTL_WEBFETCH_RELAY_URL` | Optional adapter-to-relay origin; defaults to the public relay origin. HTTPS or loopback HTTP only |

The self-host Compose file forwards these settings: provide the seed through a
protected environment file, and it reuses `PORTAL_PUBLIC_ORIGIN` plus the relay
container's loopback endpoint. For example, run Compose with both
`--env-file .env --env-file /secure/webfetch.env`. Keep using the same protected
file on subsequent deployments so an omitted seed does not disable the adapter.

Keep the seed stable while delegations are active. Domain-separated derivation
produces a controller identity and a relay credential for each request. Browser
pages receive a temporary browser ticket, never the reusable seed, private key,
owner token, portal token or raw delegated relay credential. PostgreSQL stores
credential hashes.

The public `/webfetch` endpoint must be reachable by the web AI's fetch service.
Its calls cannot depend on the owner's browser cookies. Owner approval remains
on the authenticated portal and uses its existing CSRF protection.

Exclude **both access logs and request-bearing error logs** for `/webfetch/` at
your ingress: paths contain bearer tickets and query strings contain tool
arguments. For example, use an ingress-specific redacted log format, or scoped
`access_log off` and `error_log /var/log/nginx/webfetch.error.log crit` in nginx.
Do not disable diagnostic logs globally. Application logs never record tickets
or relay credentials. Responses use `no-store`, `no-referrer` and `noindex`;
third-party fetch-provider retention is outside wanctl's control.

The adapter is a trusted controller endpoint: it sees commands and returned
data. Controller-to-device traffic retains wanctl's mutual TLS; this is not
end-to-end encryption from the web model through an unreadable adapter.

## GET tool protocol

Default responses are static HTML with visible structured data. Add
`format=json` for JSON. There is no JavaScript or streaming requirement.

Every document identifies `protocol: "wanctl.webfetch.v1"`, a workflow `status`,
and `http_status`. For normal HTML GETs, client errors are readable HTTP 200
documents with `status: "error"` and the actual `http_status` (400, 403, 409 or
429). Many URL extractors otherwise discard the error body. **Loading a page
successfully is not authorization or tool success.** JSON preserves normal
HTTP error codes. HEAD, unsupported methods and cross-origin browser requests
also retain their HTTP errors. All authorization checks still run before any
operation or result is exposed.

### Discovery and independent requests

`GET /webfetch/v1` (also `/webfetch`) is public, static and credential-free.
It also advertises `owner_start_url` for clients without a secure random generator.
It returns `start_url_template: https://RELAY/webfetch/new/{client_nonce}`.
`GET /webfetch/new/CLIENT_NONCE` creates a pending request and returns its
`approval_url`, `status_url` and `continuation_prompt`. Templates must be filled
before fetching; literal placeholders are rejected without creating requests.

A shared public entry must not mint a reusable session URL: third-party
extractors can replay their cached content despite `no-store`. Each conversation
therefore chooses its own fresh fetch URL first. The server generates the actual
secret ticket independently; repeating a nonce at the origin cannot retrieve
an existing approved ticket. Never reuse another conversation's nonce, approval
link or status URL. Clients must not search for session URLs in public indexes.

Pending requests are approved only through the authenticated owner portal.
Approval binds the request to that owner; another account cannot take it over.
Existing `/webfetch/s/TICKET` sessions continue to work until expiry or revocation.

The approved manifest returns a `call_endpoint`; construct:

```text
GET CALL_ENDPOINT?rid=UNIQUE_REQUEST&tool=TOOL&target=CANONICAL_TARGET&...
```

`CANONICAL_TARGET` is the complete `namespace/device_id` string supplied in
`devices[].target`, not a bare namespace, bare ID or `namespace:ID`. URL-encode
each value once; the slash in a target becomes `%2F`. Each tool includes
`input_schema` and `call_url_template` for clients to discover required fields,
types, limits and allowed target values. Templates contain non-executable
placeholders rather than runnable sample commands. The available tools are:

| Tool | Parameters | Result |
| --- | --- | --- |
| `exec` | `command`, optional `cwd`, optional `timeout_seconds` | One-shot execution, exit code, bounded stdout/stderr |
| `write_text` | `path`, `content` | wanctl file upload, byte count and SHA-256 |
| `read_text` | `path` | wanctl file download, UTF-8 contents, byte count and SHA-256 |

The response contains a `job_id` and `result_url`. Running jobs additionally
return a fresh `next_url`; read that URL until `done`, `failed` or `unknown`.
Execution is asynchronous in the adapter but uses normal synchronous, one-shot
wanctl operations; it does not expose device-side persistent shells or detached
async jobs to delegated clients.

A failed job can contain `result.pairing_url`; approval of device access does
not imply the new controller has paired. Give that link and the current
`continuation_prompt` to the owner, then stop. Validation errors explain the
rejected input and point to the authorized manifest, without creating a job.
Do not enumerate guessed target formats or convert GET to POST. A file write
requires `content`, including an explicit `content=` when writing an empty file.
The `pairing_required` result confirms `execution_started: false`. After the
owner pairs, use a **new rid** for a new attempt; the completed failure remains
immutable and reusing its rid will not execute the operation.

`rid` is scoped to the grant. Reusing it with identical parameters returns the
same job; changing parameters returns 409. The durable ledger records the job
before dispatch, so repeated fetches and an adapter restart never automatically
repeat an operation. An interrupted call may have produced a side effect even
without a result: `unknown` means the owner must inspect the device before
deciding whether to try a new request. This is not a claim of exactly-once
execution of arbitrary external effects.

Limits: pending requests expire after 10 minutes; approved grants last 1–60
minutes on up to 16 devices; each grant allows 64 jobs; calls allow 1–60 seconds including queue
time (default 30); four operations run concurrently; URLs are capped at 8 KiB;
writes at 2 KiB UTF-8; reads at 32 KiB; exec captures at most 16 KiB each of
stdout and stderr and cancels on overflow. `HEAD` cannot create or execute tasks.

Browser tickets have an immutable 70-minute envelope. Inactive grants and their
task contents are removed after at least 24 hours; old browser URLs cannot
recreate deleted grants. Existing account/device audit is retained separately.

Session/status URLs are short-lived bearer credentials. Do not publish an
active conversation containing them; revoke the grant before sharing a transcript.
Operational logs record server-generated grant/job IDs and fixed rejection
categories, never browser tickets, full URLs, commands, file contents or client
request IDs. Combine those logs with the existing task ledger to distinguish
missing requests, input rejection, missing pairing and actual device failures.

## Authorization and cancellation boundaries

Delegated tokens are checked by the relay on discovery, canonical target
resolution and both HTTP/WebSocket session paths. They cannot enroll devices,
impersonate the agent side, touch another credential's session, change device
management state, mint credentials or call ordinary account-management routes.

Active connections have an exact expiry deadline and revalidate authorization
every second (revocation propagation also includes store/network latency).
The device rechecks the grant after a human operation approval, before executing
or remembering a rule. A late approval cannot revive an expired/revoked grant.
Result reads require a live grant as well.

Closing a session cancels connected one-shot execution on upgraded agents. It
does not undo completed writes or guarantee control of a deliberately detached
background process created by an otherwise authorized command.

**Before rolling a relay back to a version without this feature, revoke all
`kind='delegated'` tokens.** Older namespace-only token resolvers do not understand
the new constraints. New relays preserve delegated metadata through the upstream
inspection API and refuse to downgrade the reserved `wfd_` token prefix through
a legacy resolver.

## Development acceptance

Set `WANCTL_TEST_POSTGRES` to a disposable PostgreSQL instance to run the real
grant lifecycle, migration, encrypted controller/agent and file-operation tests.
CI provisions PostgreSQL and enables those tests by default.

`go run ./tools/webfetch-demo` provides an isolated manual/browser fixture. It
requires a private `--state-dir`, `--public-origin` and disposable PostgreSQL.
Only expose its relay `/webfetch` routes. Its owner portal is **loopback-only**
and intentionally supplies a fixed test identity; it is not a production login
configuration and must never be proxied to the public Internet. The fixture's
device uses normal policy with one test directory and one harmless command.
