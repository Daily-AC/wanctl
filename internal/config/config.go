// Package config resolves wanctl's deployment-specific settings from the
// environment or optional defaults injected at build time.
package config

import (
	"fmt"
	"os"
)

var (
	// DefaultRelay may be set at build time with -ldflags -X.
	DefaultRelay = ""
	// DefaultPortal may be set at build time with -ldflags -X.
	DefaultPortal = ""
	// DefaultLanRelay may be set at build time with -ldflags -X. Empty disables
	// the optional LAN fast path.
	DefaultLanRelay = ""
)

const (
	// DefaultTransport uses proxy-agnostic HTTP long-poll because the public
	// edge may not forward WebSocket upgrades.
	DefaultTransport = "http"
	// Portal trust is deployment data injected by the installer, never a
	// compile-time fleet root. Kept as an empty compatibility constant.
	DefaultPortalFP = ""
)

// Relay resolves the public relay URL or explains how to configure one.
func Relay() (string, error) {
	relay := EnvOr("WANCTL_RELAY", DefaultRelay)
	if relay == "" {
		return "", fmt.Errorf(`no relay configured: set WANCTL_RELAY=https://your-relay (or bake a default in at build time with -ldflags "-X wanctl/internal/config.DefaultRelay=...")`)
	}
	return relay, nil
}

// Portal resolves the portal URL or explains how to configure one.
func Portal() (string, error) {
	portal := EnvOr("WANCTL_PORTAL", DefaultPortal)
	if portal == "" {
		return "", fmt.Errorf(`no portal configured: set WANCTL_PORTAL=https://your-portal (or bake a default in at build time with -ldflags "-X wanctl/internal/config.DefaultPortal=...")`)
	}
	return portal, nil
}

// EnvOr returns the value of env var key, or def if unset/empty.
func EnvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
