// Package config holds compile-time defaults for this team's wanctl deployment,
// so end-user devices and controllers need zero environment configuration. Any
// default can be overridden by the matching WANCTL_* environment variable.
package config

import "os"

const (
	// DefaultRelay is the public relay devices and controllers reach by default.
	DefaultRelay = "https://wanctl-relay.***REMOVED***.***REMOVED***.com"
	// DefaultTransport is the proxy-agnostic transport that works through the
	// thunderbox edge (which strips WebSocket upgrades).
	DefaultTransport = "http"
	// DefaultPortal is the team web app used for OAuth enrollment and approvals.
	DefaultPortal = "https://wanctl.***REMOVED***.***REMOVED***.com"
	// DefaultLanRelay is the intranet fast-path relay (WS, WireGuard/netbird
	// mesh). Reachable only from inside the company network; agents keep a
	// second uplink to it and controllers switch to it with `wanctl net lan`.
	// Override with WANCTL_LAN_RELAY.
	DefaultLanRelay = "ws://***REMOVED-IP***:8080"
	// DefaultPortalFP is the portal's controller fingerprint. Agents pre-trust it
	// so the portal can open a console session to surface pairing/approval
	// requests on the web. Overridable via WANCTL_PORTAL_PK; if the portal's
	// identity is ever regenerated, update this. Stable across portal redeploys
	// (identity persists on the portal's /data volume).
	DefaultPortalFP = "SHA256:e+gYUIcZfC7HcEGiQyMhimwE8LK+67ocG7pUKYrw4TI="
)

// EnvOr returns the value of env var key, or def if unset/empty.
func EnvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
