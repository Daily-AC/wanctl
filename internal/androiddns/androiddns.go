// Package androiddns gives the Android build a DNS resolver that actually
// resolves.
//
// Android has no /etc/resolv.conf: name resolution goes through netd, which the
// platform exposes only via bionic's libc. A cgo-enabled binary picks that up
// for free, but wanctl ships CGO_ENABLED=0 static binaries on every platform,
// so the Go resolver falls back to its compiled-in default of 127.0.0.1:53 and
// every lookup dies with "connection refused" before a single byte reaches the
// relay. Measured on an Android 16 arm64 device, 2026-08-05:
//
//	lookup wanctl-relay.***REMOVED***.***REMOVED***.com on [::1]:53: read: connection refused
//
// The alternatives were an NDK/cgo release toolchain (drags a C cross-compiler
// into the release pipeline for one platform, and gives up static linking) or
// DNS-over-HTTPS against an IP literal (works, but adds an HTTP dependency to
// the bottom of the network stack). Pointing the pure-Go resolver at explicit
// nameservers is the smallest change that restores resolution, and it stays
// inside the resolver Go already ships.
//
// getprop net.dns1 is deliberately not consulted: netd stopped publishing it
// around Android 8, and it reads empty on every modern device (verified empty
// on the Android 16 probe above). A value that is absent exactly when you need
// it is worse than no lookup at all.
//
// The selection logic lives here without a build tag so it is testable on any
// host; only the DefaultResolver swap in install_android.go is Android-only.
package androiddns

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

// FallbackNameservers are used when nothing on the device tells us better. Two
// public resolvers with different operators and jurisdictions, so one being
// blocked or hijacked does not take resolution down: AliDNS first because
// wanctl's relay and its users are in mainland China, where it is both closest
// and least likely to be interfered with.
var FallbackNameservers = []string{"223.5.5.5:53", "8.8.8.8:53", "1.1.1.1:53"}

// EnvVar lets an operator override the nameservers, which is the escape hatch
// for a device that must resolve private names (a corporate split-horizon zone,
// a VPN-only relay hostname). Comma-separated; a bare address gets :53.
const EnvVar = "WANCTL_DNS"

// Nameservers decides which DNS servers this Android device should use, in
// descending order of how much it knows about the network:
//
//  1. WANCTL_DNS — an explicit operator decision, always wins.
//  2. /etc/resolv.conf — if a future Android, a proot distro, or a rooted setup
//     provides one, the stock Go resolver already handles it correctly, so we
//     return nil and leave net.DefaultResolver alone.
//  3. $PREFIX/etc/resolv.conf — Termux keeps its resolv.conf under its own
//     prefix, where Go never looks. Reading it honours whatever the user
//     configured for their Termux environment.
//  4. FallbackNameservers.
//
// A nil return means "change nothing".
func Nameservers(getenv func(string) string, readFile func(string) ([]byte, error)) []string {
	if v := strings.TrimSpace(getenv(EnvVar)); v != "" {
		if servers := parseList(v); len(servers) > 0 {
			return servers
		}
	}
	if _, err := readFile("/etc/resolv.conf"); err == nil {
		return nil
	}
	if prefix := strings.TrimSpace(getenv("PREFIX")); prefix != "" {
		if b, err := readFile(prefix + "/etc/resolv.conf"); err == nil {
			if servers := parseResolvConf(string(b)); len(servers) > 0 {
				return servers
			}
		}
	}
	return FallbackNameservers
}

// parseList splits a comma-separated WANCTL_DNS value into dial targets.
func parseList(v string) []string {
	var out []string
	for _, part := range strings.Split(v, ",") {
		if s := withPort(strings.TrimSpace(part)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// parseResolvConf extracts the nameserver lines from a resolv.conf body,
// ignoring comments and the options/search directives the Go resolver would
// otherwise apply — we only need somewhere to send the query.
func parseResolvConf(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if i := strings.IndexAny(line, "#;"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		if s := withPort(fields[1]); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// withPort appends the DNS port to a bare address, leaving host:port and
// bracketed IPv6 literals alone.
func withPort(addr string) string {
	if addr == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	if strings.Count(addr, ":") >= 2 && !strings.HasPrefix(addr, "[") {
		return "[" + addr + "]:53" // bare IPv6 literal
	}
	return addr + ":53"
}

// dialFunc matches net.Dialer.DialContext so tests can observe which server a
// query was actually sent to.
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Resolver builds a pure-Go resolver that talks to the given servers.
func Resolver(servers []string) *net.Resolver {
	return resolverWith(servers, func(ctx context.Context, network, addr string) (net.Conn, error) {
		d := net.Dialer{Timeout: 5 * time.Second}
		return d.DialContext(ctx, network, addr)
	})
}

// resolverWith is Resolver with an injectable dialer.
//
// Go's resolver calls Dial once per query attempt and retries on failure, so
// rotating the server on each call turns those built-in retries into real
// failover across operators rather than three tries at the same dead address.
func resolverWith(servers []string, dial dialFunc) *net.Resolver {
	if len(servers) == 0 {
		return nil
	}
	var next atomic.Uint64
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			server := servers[int((next.Add(1)-1)%uint64(len(servers)))]
			return dial(ctx, network, server)
		},
	}
}
