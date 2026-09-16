# WebFetch access

WebFetch lets a web AI that can read URLs use wanctl without an MCP connector.
It is an optional controller adapter hosted alongside the relay. The owner
approves a short-lived delegation in the existing wanctl portal; device trust,
rules, mode and per-request approvals still decide what runs.

## Owner workflow

1. Ask the AI to open `https://RELAY/webfetch`, then open its `start_url`.
2. The AI returns an `approval_url`. Open it yourself, sign in to wanctl,
   verify the controller and selected device identities, choose your devices
   and a duration, and approve. Fetching this URL cannot approve a request.
3. Ask the AI to reread its `status_url`. The approved response contains the
   exact allowed device targets and tool parameters.
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

The approved manifest returns a `call_endpoint`; construct:

```text
GET CALL_ENDPOINT?rid=UNIQUE_REQUEST&tool=TOOL&target=CANONICAL_TARGET&...
```

URL-encode every parameter. The available tools are:

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
