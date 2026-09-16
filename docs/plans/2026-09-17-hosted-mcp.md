# Hosted MCP endpoint

Status: shipped to the z10 deployment 2026-09-17. Branch `feat/hosted-mcp`.

## Why now

The relay has carried a Streamable-HTTP MCP server since the MCP work landed:
`main.go` turns it on when `WANCTL_MCP_SEED` is set and mounts it at
`/wanctl-mcp`. It was never switched on anywhere. The 2026-09-05 owner-devices
migration recorded the reason as minimum exposure — there was no AI host that
needed it, because every host in use could start a local `wanctl` process and
talk to it over stdio.

That changed. Hosts that cannot spawn a subprocess — a browser chat, a cloud
agent runner — now need to reach the same tools, and stdio cannot serve them.
Minimum exposure was the right default while nothing needed the surface; it
stops being a reason once something does.

## What was missing

Only wiring. No Go change was needed for the endpoint itself.

- `selfhost/docker-compose.yml` never passed `WANCTL_MCP_SEED` to the relay
  container, so a self-hoster had no supported way to enable it.
- The relay container also had no `WANCTL_PORTAL` and no `WANCTL_RELAY`. Both
  matter: an MCP session is a *controller*, and `wanctl_login` resolves the
  portal origin for the enrollment URL while every data tool resolves the relay
  URL to reach the broker. Without them the endpoint answers `initialize` and
  `tools/list` and then fails at the first login with `no portal configured`.
  This was measured, not assumed: a local `wanctl mcp --http` with the seed
  alone returns exactly that error.
- No user-facing documentation for either transport.

`WANCTL_RELAY` is set to the container's own loopback (`http://127.0.0.1:8080`),
matching `WANCTL_WEBFETCH_RELAY_URL`, so MCP traffic does not hairpin out
through the public edge and back.

## What shipped

- Compose passes `WANCTL_MCP_SEED`, `WANCTL_MCP_ALLOW_UNSAFE_TRUST_SERVER`,
  `WANCTL_PORTAL` and `WANCTL_RELAY` to the relay. The two seeds default to
  empty, which is off.
- `selfhost/.env.example` documents both opt-ins.
- `docs/self-hosting.md` (and `.zh.md`) gained "Optional: enable the hosted MCP
  endpoint".
- `docs/portal/ai__mcp.md` (and `.en.md`) is the user guide: when to use stdio,
  when to use the hosted endpoint, the registration commands for both, the
  two-step portal login, the first-contact pairing approval, what a rebind
  credential is, and the security boundary.
- The Feishu wording in the MCP tool descriptions and the login prompt is now
  portal-neutral. The portal's identity provider is deployment configuration;
  the tool text was telling every user to look for a Feishu button.

Also fixed, because they broke `tools/docsite/build.py` on `main` and blocked
publishing anything: `docs/architecture.zh.md` was missing the device-identity
link its source carries, and three documents linked to `webfetch.md`, which the
docs site does not publish. The webfetch links now point at the repository copy,
and `device-identity.md` joined the site.

## Known limit

A hosted session keeps device trust in an in-memory store, and
`wanctl_trust_server` is fail-closed unless the operator sets
`WANCTL_MCP_ALLOW_UNSAFE_TRUST_SERVER=1`. So out of the box a hosted session can
log in and list devices but cannot run anything: the first `wanctl_exec` stops
at `DEVICE IDENTITY CONFIRMATION REQUIRED`. Whether to accept the weaker flow on
a relay you own yourself is an owner decision, so this deployment ships with the
opt-in unset and the endpoint read-only. Both documents say so.

## Acceptance (z10 deployment, 2026-09-17)

- `initialize` over `https://wanctl-relay.z10.dev/wanctl-mcp` returns 200 with
  `serverInfo.name == "wanctl"` and an `Mcp-Session-Id` header.
- `tools/list` returns 17 tools including `wanctl_login`, `wanctl_peers` and
  `wanctl_exec`.
- `wanctl_login` with no arguments returns the portal enroll URL.
- Claude Code registered with `--transport http` connects and calls the tool.
- A real portal code exchanged through `wanctl_login` bound the session to its
  namespace and `wanctl_peers` listed all three online devices.
- Relay logs `MCP server enabled at /wanctl-mcp`; the postgres container was not
  restarted; both `/healthz` endpoints answer `ok`; the local stdio path is
  unaffected.

## Rollback

Delete the `WANCTL_MCP_SEED=` line from `selfhost/.env` and recreate the relay:

```bash
docker compose up -d --no-deps relay
```

The endpoint goes back to 404 and every hosted session and rebind credential
dies with it. Nothing else on the relay changes. `--no-deps` keeps the postgres
container out of the recreate.
