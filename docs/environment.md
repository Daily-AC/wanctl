# Environment variable reference

This reference is derived from environment reads in the production Go source
and its opt-in live tests. Empty values normally behave like unset values.
Variables marked "conditional" are required only for the feature described.

## Server and container

| Variable | Role | Required | Default | Purpose |
|---|---|---:|---|---|
| `WANCTL_ROLE` | container | No | `relay` | Docker image command: `relay`, `portal`, or `mcp`. |
| `DATABASE_URL` | relay | Conditional | none | PostgreSQL DSN. Required for the portal-backed multi-user deployment; otherwise relay needs `WANCTL_TOKENS` or `WANCTL_UPSTREAM_RELAY`. |
| `WANCTL_AUTO_MIGRATE` | relay | No | enabled | Set to `0` to skip embedded database migrations. |
| `WANCTL_ADMIN_SECRET` | relay, portal, admin CLI | Conditional | none | Shared secret for `/admin/*`. Required for a functional portal, upstream token resolution, admin CLI, and server-log access. |
| `WANCTL_TOKENS` | relay | Conditional | none | Static `token:namespace` pairs separated by commas; fallback for a relay without Postgres. |
| `WANCTL_UPSTREAM_RELAY` | relay | Conditional | none | Upstream relay URL used to resolve tokens when this relay has no database. Requires `WANCTL_ADMIN_SECRET`. |
| `WANCTL_PORTAL_NS` | relay | No | none | Namespace allowed to open privileged portal console sessions. Conventionally `portal`. |
| `WANCTL_DIST_DIR` | relay | No | `/dist` | Directory containing signed release artifacts and installers. |
| `WANCTL_PUBLIC_ORIGIN` | relay | No | request origin | Canonical relay origin substituted into the served `/skills` document. |
| `WANCTL_MCP_SEED` | relay, MCP | Conditional | none | Hex seed enabling `/wanctl-mcp` on relay; required and at least 32 decoded bytes for standalone `mcp --http`. |
| `RELAY_ADMIN_URL` | portal | Yes | none | Internal relay base URL used for the portal's admin proxy, such as `http://relay:8080`. |
| `WANCTL_GITHUB_CLIENT_ID` | portal | Conditional | none | Enables GitHub OAuth login. Mutually exclusive with `PORTAL_USER_HEADER`. |
| `WANCTL_GITHUB_CLIENT_SECRET` | portal | Conditional | none | OAuth App client secret; required when the client ID is set. |
| `WANCTL_SESSION_SECRET` | portal | Conditional | none | HMAC key for OAuth state and session cookies; at least 32 bytes when OAuth is enabled. |
| `WANCTL_GITHUB_AUTH_BASE` | portal | No | `https://github.com` | OAuth authorization/token base URL; useful for GitHub Enterprise. |
| `WANCTL_GITHUB_API_BASE` | portal | No | `https://api.github.com` | GitHub user API base URL; useful for GitHub Enterprise. |
| `PORTAL_USER_HEADER` | portal | Conditional | `X-Auth-Request-Email` | Trusted reverse-proxy identity header for header-auth mode. The proxy must strip client-supplied copies. Mutually exclusive with GitHub OAuth. |
| `PORTAL_PUBLIC_ORIGIN` | portal | No | derived from request | External portal origin used for OAuth redirects and secure cookies. Set it when TLS terminates at a proxy. |
| `PORTAL_DEBUG_WHOAMI` | portal | No | `0` | Set to `1` to enable the diagnostic `/whoami` endpoint. Do not enable routinely. |
| `WANCTL_RELAY` | portal | Conditional | persisted config, then build-time default | Public relay URL used by the portal console and `/skills` redirect. Also used by clients and agents, who can persist it with `wanctl config set relay=…`. |
| `WANCTL_PORTAL_TOKEN` | portal | Conditional | none | Token in `WANCTL_PORTAL_NS`; required only for the live device console. |
| `WANCTL_TRANSPORT` | portal, agent, controller, MCP | No | `http` | Carrier: proxy-agnostic `http` long-poll or `ws`. |
| `WANCTL_CONFIG_DIR` | all stateful roles | No | OS user config directory | Directory for identity, trust, token, label, logs, and process state. The container image sets `/data`. |
| `WANCTL_LARK_APP_ID` | portal | No | none | Legacy optional Lark approval integration; effective only when the matching secret is also set. |
| `WANCTL_LARK_APP_SECRET` | portal | No | none | Secret paired with `WANCTL_LARK_APP_ID`. |

## Agent and controller

| Variable | Role | Required | Default | Purpose |
|---|---|---:|---|---|
| `WANCTL_PORTAL` | agent, controller | Conditional | persisted config, then build-time default | Portal URL used for login/enrollment and pairing links. Persist with `wanctl config set portal=…`. |
| `WANCTL_RELEASE_BASE` | agent, controller, installers | No | build-time default or none | Base URL where signed release artifacts live flat (official builds bake the project's GitHub releases). `wanctl update` and the installers pull from it; empty falls back to the relay's `/dl` mirror. |
| `WANCTL_DIST_BASE` | installers | No | none | Installer-only override of the artifact source; wins over `WANCTL_RELAY` and the baked release base. |
| `WANCTL_TOKEN` | agent, controller, MCP | Conditional | saved token or none | Namespace bearer token. Overrides the token stored in the config directory. |
| `WANCTL_LABEL` | controller, MCP | No | saved label or generated MCP label | Human-readable controller identity shown during pairing. |
| `WANCTL_LAN_RELAY` | agent, controller | No | build-time default or none | Optional intranet WebSocket relay used by the LAN fast path. |
| `WANCTL_PORTAL_FPS` | agent | No | none | Comma-separated portal administrator fingerprints to seed. |
| `WANCTL_PORTAL_FP` | agent | No | none | Deprecated single-fingerprint alias, used only when `WANCTL_PORTAL_FPS` is unset. |

## Android-specific

| Variable | Role | Required | Default | Purpose |
|---|---|---:|---|---|
| `WANCTL_DNS` | Android agent/controller | No | system/Termux resolver, then built-in public resolvers | Comma-separated DNS server addresses; bare addresses use port 53. |
| `WANCTL_ELEVATION` | Android agent | No | disabled | Truthy value (`1`, `true`, `yes`, or `on`) enables Android `su`/ADB elevation channels. |
| `WANCTL_ADB_PORT` | Android agent | No | discovered port, then `5555` | Comma-separated local adbd ports to try before other sources. |
| `WANCTL_DEVICE_NAME` | Android agent | No | `wanctl-agent` for ADB identity | Name embedded in the local ADB key label. The Android app normally injects it. |
| `WANCTL_DEVICE_STATE_FILE` | Android agent | No | none | JSON state file written by the Android app for battery data and adbd discovery; normally injected by the app. |

## Release tooling

| Variable | Role | Required | Default | Purpose |
|---|---|---:|---|---|
| `WANCTL_RELEASE_SIGNING_KEY` | release manifest tool | Conditional | none | Base64 Ed25519 seed or private key; required to sign a release. |
| `WANCTL_RELEASE_RSA_KEY` | release manifest tool | Conditional | none | Base64 PKCS#8 or PKCS#1 RSA private key (at least 2048 bits) used for installer signatures. |

## Opt-in live tests

These variables are not runtime service configuration. They keep tests that
contact real external systems disabled unless an operator supplies all required
values explicitly.

| Variable | Role | Required | Default | Purpose |
|---|---|---:|---|---|
| `WANCTL_LIVE_RELAY` | client live test | Conditional | none | Relay URL for the remote-console end-to-end test. |
| `WANCTL_LIVE_DEVTOK` | client live test | Conditional | none | Token for the test device namespace. |
| `WANCTL_LIVE_PORTALTOK` | client live test | Conditional | none | Privileged portal namespace token. |
| `WANCTL_LIVE_TARGET` | client live test | No | `alice/macbox` | Fully qualified target used by the live test. |
| `WANCTL_LARK_TEST_EMAIL` | Lark live test | Conditional | none | Recipient of the real approval-card probe. |
| `WANCTL_LARK_KEEP_CARD` | Lark live test | No | `0` | Set to `1` to leave the probe card actionable instead of resolving it immediately. |
| `WANCTL_LARK_LIVE_SECONDS` | Lark live test | Conditional | none | Positive number of seconds to keep the real callback consumer connected. |

Standard process variables such as `EDITOR`, `PREFIX`, and `TMPDIR` are outside
the wanctl-specific API.
