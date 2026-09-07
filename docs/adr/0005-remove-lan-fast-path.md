# ADR 0005 — The LAN fast path is removed; intranet latency is a relay you host

Status: accepted (2026-09-07)

## Context

Since early on, a device could hold a **second** uplink to an intranet
WebSocket relay alongside its public one, and a controller could be pointed at
that second relay with `wanctl net wan|lan|auto|status`. It cost a
build-time default (`DefaultLanRelay`), an env var (`WANCTL_LAN_RELAY`), an
agent flag (`--lan-relay`), two persisted files in the config directory
(`netmode`, `lan`), a reconnect loop with its own backoff and kick channel, a
protocol kind (`lan_set`), a field in the console state snapshot, a portal API
endpoint, and a switch in the portal UI.

Nobody uses it. No public deployment enables it, and the owner's own fleet runs
in `wan` mode with no intranet relay configured — so the code path that would
prove the feature works has never run outside its tests. The security audit of
2026-08-28 recorded the same thing as ARCH-G-08: a third token-store and uplink
surface that no deployment turns on, with the disposition left to the owner.

The feature is also redundant. A developer who wants intranet latency can run a
relay on the intranet and point devices and controllers at it with
`wanctl config set relay=…`. That is one URL, not a second uplink, and it
already works: a relay without a database resolves tokens against the main
relay (`WANCTL_UPSTREAM_RELAY` + `WANCTL_ADMIN_SECRET`), so portal-issued
tokens are valid there too. The fast path bought nothing a relay URL does not.

## Decision

**Remove the LAN fast path entirely.** The agent has one uplink, the controller
resolves one relay, and the console state describes one connection. Issue #13
("Split wanctl into a core and plugins") was closed on the same reasoning: the
LAN fast path is a transport choice, not an integration, and a developer who
wants intranet latency runs a relay on the intranet.

**Leftover configuration is ignored, not rejected.** An upgraded install may
still have a `netmode` file reading `lan` or `auto` and a device-side `lan`
switch in its config directory, and an old service unit may still export
`WANCTL_LAN_RELAY`. Nothing reads any of them any more, so a controller that
carries them starts normally and resolves its ordinary relay setting. Refusing
to start, or migrating the files, would both be more code for a state that
resolves itself. `TestLegacyLanConfigIsIgnored` pins that.

**Old agents stay compatible.** An agent on an older build still reports a
`lan` object inside its console-state snapshot; the portal decodes that
snapshot with the standard `encoding/json` decoder, which drops unknown fields,
and no console path uses `DisallowUnknownFields`. An old portal that sends
`lan_set` to a new agent gets the ordinary `unknown RPC kind` error rather than
a hang.

## Consequences

- Intranet latency is now a deployment choice, not a product feature: run a
  relay inside the network (see `docs/architecture.md`, "Satellite relays") and
  point the devices at it. One relay per device, chosen by configuration.
- A device can no longer be reachable over the intranet *and* the public relay
  at the same time. Nothing depended on that: the feature was never enabled
  anywhere, and E2E trust was identical on both uplinks anyway, so the only
  loss is an automatic failover nobody had turned on.
- `wanctl net` no longer exists. It was the only command the feature exposed,
  and it now reports an unknown command with the usual usage text.
- ARCH-G-08 from the 2026-08-28 security audit is resolved by removal. The
  audit document itself is a historical record and is left as it stands.

Reverting is a `git revert` of this PR while it is still recent; nothing in the
removal migrates data or changes a wire format in a way that would survive the
revert.
