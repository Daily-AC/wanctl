// Package config holds compile-time defaults for this team's wanctl deployment,
// so end-user devices and controllers need zero environment configuration. Any
// default can be overridden by the matching WANCTL_* environment variable.
package config

import "os"

const (
	// DefaultRelay is the public relay devices and controllers reach by default.
	DefaultRelay = "https://wanctl-relay.***REMOVED***.***REMOVED***.com"
	// DefaultTransport uses proxy-agnostic HTTP long-poll because the public
	// Thunderbox edge does not forward WebSocket upgrades.
	DefaultTransport = "http"
	// DefaultPortal is the team web app used for OAuth enrollment and approvals.
	DefaultPortal = "https://wanctl.***REMOVED***.***REMOVED***.com"
	// DefaultLanRelay is the intranet fast-path relay. Its ws:// admission header
	// is safe only because this address is expected to run inside the encrypted
	// WireGuard/netbird mesh; deployments without that boundary must override it
	// with wss://. Reachable only from inside the company network; agents keep a
	// second uplink to it and controllers switch to it with `wanctl net lan`.
	// Override with WANCTL_LAN_RELAY.
	DefaultLanRelay = "ws://***REMOVED-IP***:8080"
	// Portal trust is deployment data injected by the installer, never a
	// compile-time fleet root. Kept as an empty compatibility constant.
	DefaultPortalFP = ""
)

// EnvOr returns the value of env var key, or def if unset/empty.
func EnvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
