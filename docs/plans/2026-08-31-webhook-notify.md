# Webhook notifications

Status: implemented (2026-08-31)

wanctl sends account notifications to Feishu group robots, DingTalk group
robots, or a generic JSON receiver. The relay is the only webhook sender.

## Why the relay sends everything

The original design split delivery between the device and relay according to
where an event was observable. The final design still observes events at the
same place, but devices report their events to the relay and the relay performs
every outbound webhook request.

This gives the deployment one rate limiter, one delivery-health record, and one
source IP for robot IP allow-lists. Devices need network access only to the
relay they already use, so routers and restricted corporate hosts do not need a
new outbound path to Feishu, DingTalk, or a custom receiver. Webhook URLs and
signing secrets never leave the relay.

## Configuration

The destination is account-wide and each owned device has a default-off switch.
The account configuration contains:

- `url`: the credential-bearing robot or custom webhook URL.
- `format`: `feishu`, `dingtalk`, or `json`.
- optional `keyword`: always included in the message title for robot keyword
  security checks.
- optional `secret`: supported for both DingTalk and Feishu signing. The two
  constructions are deliberately different; see Wire formats.
- four event-class switches: approval, exec, lifecycle, and security.
- `exec_failures_only`, default true.
- `include_detail`, default false.

The URL is returned masked as scheme, host, and the final four characters. The
secret is never returned; reads expose only `secret_set`. A POST may omit URL or
secret to preserve the stored value, send an empty secret to clear it, or use
`delete:true` to remove the account configuration.

Migration `004_notify.sql` creates these tables:

```sql
CREATE TABLE notify_webhook (
    namespace text PRIMARY KEY,
    url text NOT NULL,
    format text NOT NULL DEFAULT 'json',
    keyword text NOT NULL DEFAULT '',
    secret text NOT NULL DEFAULT '',
    on_approval boolean NOT NULL DEFAULT true,
    on_exec boolean NOT NULL DEFAULT false,
    on_lifecycle boolean NOT NULL DEFAULT true,
    on_security boolean NOT NULL DEFAULT true,
    exec_failures_only boolean NOT NULL DEFAULT true,
    include_detail boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE device_notify (
    namespace text NOT NULL,
    device text NOT NULL,
    enabled boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, device)
);

CREATE TABLE notify_health (
    namespace text NOT NULL,
    device text NOT NULL,
    attempted_at timestamptz NOT NULL,
    result text NOT NULL,
    http_status integer NOT NULL DEFAULT 0,
    provider_code text NOT NULL DEFAULT '',
    error text NOT NULL DEFAULT '',
    consecutive_failures integer NOT NULL DEFAULT 0,
    PRIMARY KEY (namespace, device)
);
```

An empty health `device` value denotes relay-only account events and test
notifications. Registered device names cannot be empty.

## Event ownership

| Event | Observer | Reporter |
| --- | --- | --- |
| `approval.pending` | device | agent |
| `exec.finished` | device | agent |
| `pairing.requested` | device | agent |
| `trust.changed` | device | agent |
| `device.online` / `device.offline` | relay | relay |
| `enroll.completed` | relay | relay |
| `friend.requested` | relay | relay |

Device events use `POST /agent/events`. Admission authentication supplies the
namespace, while the `device` and random agent `inst` query values must match a
currently live WS or HTTP registration. A namespace token without the live
instance ID cannot report as another device. The request body deliberately has
no device field, and unknown fields are rejected.

Each report carries a stable event ID. The relay keeps a ten-minute
namespace/device/event-ID dedupe window, so HTTP retries and control-channel
reconnections cannot produce duplicate notifications. The table is capped at
10,000 entries with oldest-entry eviction. Agent retries reuse the same
serialized report.

Both agent transports use the same HTTPS reporting endpoint. A WS registration
includes the instance ID; HTTP long-poll registration already carries it.

`GET /agent/notify-policy` returns only `include_detail`. It is also bound to the
live instance and never returns URL, format, keyword, or secret.

## Detail privacy

`include_detail` is false by default. In that state, the agent serializes only
the event ID, event type, timestamp, and exec exit status. It removes command
lines, file paths, peer text, and other device content before making the relay
request. This keeps `internal/eventlog`'s device-content isolation true on the
default path.

When the owner explicitly enables details, the agent applies
`eventlog.RedactText` before serialization. The relay applies it again while
building the outbound payload. A failed policy refresh fails closed by setting
`include_detail` back to false.

## Device and account switches

Device-bound events require both an account destination and that device's
`device_notify.enabled` switch. Missing rows are off.

`friend.requested` has no device, and a first `enroll.completed` occurs before a
new device can have a switch. Those two events therefore use the account
security switch without a device switch and record health under the relay-only
key. This is the intentional exception to per-device gating.

## Wire formats

All formats POST `application/json`.

### Generic JSON

```json
{"event":"approval.pending","namespace":"alice","device":"legion",
 "ts":"2026-08-31T10:40:00Z","message":"legion has a command waiting for approval",
 "detail":"deploy --token [REDACTED]","peer":"macbook","exit":null}
```

Optional fields are absent when details are disabled.

### Feishu

The relay sends an interactive card:

```json
{"msg_type":"interactive","card":{
  "config":{"wide_screen_mode":true},
  "header":{"title":{"tag":"plain_text","content":"WANCTL - wanctl approval.pending - legion"},"template":"red"},
  "elements":[
    {"tag":"div","text":{"tag":"lark_md","content":"..."}},
    {"tag":"note","elements":[{"tag":"plain_text","content":"legion - local time"}]}
  ]
}}
```

Approval and security events use a red header, lifecycle uses blue, and exec
uses grey. This shape follows the production `notifyd/server.mjs` implementation.
Feishu can return HTTP 200 for a rejected message; a present non-zero `code` is
treated as failure and preserved in health.

### DingTalk

The relay sends a markdown robot message with a top-level `msgUuid`:

```json
{"msgtype":"markdown","markdown":{"title":"WANCTL - wanctl device.online - legion","text":"..."},"msgUuid":"..."}
```

One `msgUuid` is generated per logical Send and reused for every retry. HTTP 200
is successful only when `errcode` is present and zero. Non-zero values,
including security error `310000`, preserve `errcode` and the sanitized
`errmsg` in delivery feedback.

DingTalk signing uses:

```text
stringToSign = timestampMillis + "\n" + secret
signature = Base64(HMAC-SHA256(key=secret, data=stringToSign))
```

The signature is URL-encoded and appended with `timestamp` to the configured
robot URL. A fixed timestamp/secret test vector locks the key/data direction.

## Delivery controls

- URLs must be HTTPS. DingTalk URLs must use
  `oapi.dingtalk.com/robot/send` with an `access_token` query value.
- The relay resolves destinations and rejects loopback, private, link-local,
  unspecified, carrier-NAT, and other non-public addresses. The checked address
  is also pinned at dial time to resist DNS rebinding and redirects.
- The account-wide limit is 15 notifications per minute across all devices.
  The sixteenth event is dropped and replaced by one `notify.throttled`; later
  events in the same window are silently dropped.
- Delivery uses a five-second HTTP timeout and two exponential-backoff retries.
  Provider validation errors and other non-retryable 4xx results are not retried.
- Notification reporting and sending are asynchronous to approval, exec,
  pairing, and trust flows.

Fixed relay egress makes a Feishu or DingTalk robot IP allow-list the recommended
security setting. Keyword checks are supported in message titles, and both
providers' signing schemes are implemented.

Feishu and DingTalk sign differently, and the mistake they invite is to reuse one
construction for both — the result is a well-formed signature the server always
rejects, which looks like a configuration problem rather than a code bug:

| | DingTalk | Feishu |
| --- | --- | --- |
| HMAC key | the secret | `"{timestamp}\n{secret}"` |
| HMAC data | `"{timestamp}\n{secret}"` | the empty string |
| Timestamp unit | milliseconds | seconds |
| Signature location | URL query, URL-encoded | body fields `timestamp` and `sign` |

Both directions are pinned by unit tests against vectors computed outside this
codebase from each provider's own documented sample.

## Health and portal

Every actual delivery records the last result by namespace and device: HTTP
status, provider code, sanitized/truncated error, timestamp, and consecutive
failure count. Before persistence or logging, the relay removes the full URL,
secret, query credential values, and webhook path ID, then applies
`eventlog.RedactText` and a 512-byte cap.

The portal notification page edits the account URL, format, keyword, secret,
four event classes, exec-failure filter, and detail switch. Its test button sends
a real `notify.test` through the relay sender and displays both HTTP status and
provider `code`/`errcode`.

Each owned device's mode page shows a separate webhook switch and delivery
health beside the existing Feishu approval card. Shared-device views neither
render nor call these controls, and portal handlers enforce the same owner gate.

## Defaults and tests

The security posture remains default-off: no account URL, every device switch
off, and `include_detail` off.

Tests cover both robot body shapes, provider errors inside HTTP 200, DingTalk
retry UUID reuse and signing, account-wide rate limiting, SSRF rejection,
write-time HTTPS validation, masking and secret handling, health sanitization,
agent detail omission/redaction, notification failure independence, event
dedupe, live-instance device binding, and shared-device isolation through relay
and portal routes.
