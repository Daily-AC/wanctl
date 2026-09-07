// Package config resolves wanctl's deployment-specific settings. Precedence,
// highest first: command-line flag (applied by the caller) > environment
// variable > persisted config file (`wanctl config set …`) > optional default
// injected at build time.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

var (
	// DefaultRelay may be set at build time with -ldflags -X.
	DefaultRelay = ""
	// DefaultPortal may be set at build time with -ldflags -X.
	DefaultPortal = ""
	// DefaultLanRelay may be set at build time with -ldflags -X. Empty disables
	// the optional LAN fast path.
	DefaultLanRelay = ""
	// DefaultReleaseBase may be set at build time with -ldflags -X: the base URL
	// under which signed release artifacts live flat (manifest.json,
	// wanctl-<os>-<arch>, …). Official builds point it at the project's GitHub
	// releases so `wanctl update` needs no relay-side mirror.
	DefaultReleaseBase = ""
)

const (
	// DefaultTransport uses proxy-agnostic HTTP long-poll because the public
	// edge may not forward WebSocket upgrades.
	DefaultTransport = "http"
	// Portal trust is deployment data injected by the installer, never a
	// compile-time fleet root. Kept as an empty compatibility constant.
	DefaultPortalFP = ""
)

// Setting resolves one endpoint setting and reports where the value came from
// ("env WANCTL_…", "config file", "build default", or "" when unset). The keys
// are the ones `wanctl config` exposes: relay, portal, transport, release_base.
func Setting(key string) (value, source string) {
	envKey, def := "", ""
	switch key {
	case "relay":
		envKey, def = "WANCTL_RELAY", DefaultRelay
	case "portal":
		envKey, def = "WANCTL_PORTAL", DefaultPortal
	case "transport":
		envKey, def = "WANCTL_TRANSPORT", DefaultTransport
	case "release_base":
		envKey, def = "WANCTL_RELEASE_BASE", DefaultReleaseBase
	default:
		return "", ""
	}
	if v := os.Getenv(envKey); v != "" {
		return v, "env " + envKey
	}
	if v := StoredSetting(key); v != "" {
		return v, "config file"
	}
	if def != "" {
		return def, "build default"
	}
	return "", ""
}

// Relay resolves the public relay URL or explains how to configure one.
func Relay() (string, error) {
	relay, _ := Setting("relay")
	if relay == "" {
		return "", fmt.Errorf("no relay configured: run `wanctl config set relay=https://your-relay` (or set WANCTL_RELAY)")
	}
	return relay, nil
}

// Portal resolves the portal URL or explains how to configure one.
func Portal() (string, error) {
	portal, _ := Setting("portal")
	if portal == "" {
		return "", fmt.Errorf("no portal configured: run `wanctl config set portal=https://your-portal` (or set WANCTL_PORTAL)")
	}
	return portal, nil
}

// Transport resolves the controller/device transport: "ws" or "http". Always
// non-empty — the build default backstops it.
func Transport() string {
	t, _ := Setting("transport")
	return t
}

// ReleaseBase resolves where signed release artifacts are downloaded from:
// WANCTL_RELEASE_BASE, then `wanctl config set release_base=…`, then the build
// default. Empty means no release page is configured and callers fall back to
// the relay's /dl mirror.
//
// The persisted layer is what lets someone who cannot reach the build's release
// page — an official build bakes GitHub — point `wanctl update` at a mirror
// once instead of exporting an environment variable for every invocation.
func ReleaseBase() string {
	base, _ := Setting("release_base")
	return base
}

// RelayHTTPOrigin converts a relay's dial URL to the HTTP(S) base used by
// peers, long-poll, enrollment, and other finite HTTP requests. Parse the
// scheme rather than replacing the first "ws" substring: a hostname such as
// relay.aws.example must remain unchanged.
func RelayHTTPOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid relay URL %q", raw)
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	case "http", "https":
	default:
		return "", fmt.Errorf("unsupported relay URL scheme %q", u.Scheme)
	}
	u.RawQuery = ""
	u.Fragment = ""
	return strings.TrimRight(u.String(), "/"), nil
}

// EnvOr returns the value of env var key, or def if unset/empty.
func EnvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
