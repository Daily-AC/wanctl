// Package clientip names the client behind an HTTP request, for limits that
// are meant to apply per client rather than to everyone at once.
package clientip

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Key returns the client a request came from, in the form a per-client limit
// should be keyed on.
//
// A relay runs behind a reverse proxy on its own host (nginx, reaching the
// container through a Docker port mapping bound to loopback), so the TCP peer
// of every request is the proxy, and a limit keyed on it is one budget shared
// by the whole internet. The proxy overwrites X-Real-IP with the address it
// accepted the connection from, so that header names the client — but only
// when the peer is the proxy, which is why it is read only from a loopback or
// private peer. A client that connects directly cannot rename itself with it.
// (A relay exposed without a proxy on a private network lets its peers choose
// their bucket; the global caps next to every per-client one still hold.)
//
// X-Forwarded-For is never read: a proxy appends to whatever the client sent,
// so everything but its last entry is the client's own claim.
//
// An IPv6 address is reduced to its /64, which is what a single subscriber is
// normally handed; keying on the full address would let one host take a fresh
// budget per address.
func Key(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, ok := parse(host)
	if !ok {
		// Not an IP peer at all (a unix socket, a test harness): whatever it
		// is, it is one client.
		return r.RemoteAddr
	}
	if peer.IsLoopback() || peer.IsPrivate() {
		if real, ok := parse(r.Header.Get("X-Real-IP")); ok {
			peer = real
		}
	}
	if peer.Is6() {
		prefix, _ := peer.Prefix(64)
		return prefix.String()
	}
	return peer.String()
}

func parse(s string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap().WithZone(""), true
}
