package direct

import (
	"net"
	"net/netip"
	"sort"
)

type hostCandidate struct {
	addr         netip.Addr
	pointToPoint bool
}

// Overlay addresses are never candidates. Tailscale uses them, and on Windows
// its adapter is not flagged point-to-point, so they would be advertised. When
// Tailscale itself is relaying through DERP, a QUIC handshake over it completes
// but moves tens of kilobytes per second, far slower than the relay fallback.
// 100.64.0.0/10 is also carrier-grade NAT space, which a peer cannot reach.
var overlayPrefixes = []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("fd7a:115c:a1e0::/48")}

func usableAddr(ip netip.Addr, allowLoopback bool) bool {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return false
	}
	if allowLoopback && ip.IsLoopback() {
		return true
	}
	for _, p := range overlayPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return ip.IsGlobalUnicast() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && ip != netip.MustParseAddr("255.255.255.255")
}

func validateCandidates(raw []string, allowLoopback bool) []netip.AddrPort {
	if len(raw) > 8 {
		return nil
	}
	out := make([]netip.AddrPort, 0, len(raw))
	for _, s := range raw {
		if len(s) > 64 {
			return nil
		}
		ap, err := netip.ParseAddrPort(s)
		if err != nil || ap.Port() == 0 || !usableAddr(ap.Addr(), allowLoopback) || ap.Addr().Zone() != "" {
			return nil
		}
		out = append(out, netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()))
	}
	return out
}

// ValidateCandidates rejects the entire peer list on one invalid entry.
func ValidateCandidates(raw []string, allowLoopback bool) []netip.AddrPort {
	return validateCandidates(raw, allowLoopback)
}

func isPrivate(ip netip.Addr) bool { return ip.IsPrivate() }

func orderCandidates(port uint16, defaults []netip.Addr, reflexive []netip.AddrPort, hosts []hostCandidate, allowLoopback bool) []string {
	pairs := make([]netip.AddrPort, 0, len(defaults))
	for _, ip := range defaults {
		pairs = append(pairs, netip.AddrPortFrom(ip, port))
	}
	return orderCandidatePairs(pairs, reflexive, hosts, port, port, allowLoopback)
}

func orderCandidatePairs(defaults, reflexive []netip.AddrPort, hosts []hostCandidate, port4, port6 uint16, allowLoopback bool) []string {
	out := make([]string, 0, 8)
	seen := map[netip.AddrPort]bool{}
	add := func(ap netip.AddrPort) {
		ap = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
		if len(out) < 8 && ap.Port() != 0 && usableAddr(ap.Addr(), allowLoopback) && !seen[ap] {
			out = append(out, ap.String())
			seen[ap] = true
		}
	}
	for _, ap := range defaults {
		add(ap)
	}
	v4, v6 := 0, 0
	for _, ap := range reflexive {
		if ap.Addr().Unmap().Is4() {
			if v4 >= 2 {
				continue
			}
			v4++
		} else {
			if v6 >= 1 {
				continue
			}
			v6++
		}
		add(ap)
	}
	sort.SliceStable(hosts, func(i, j int) bool { return isPrivate(hosts[i].addr) && !isPrivate(hosts[j].addr) })
	for _, h := range hosts {
		if !h.pointToPoint {
			port := port4
			if h.addr.Is6() {
				port = port6
			}
			add(netip.AddrPortFrom(h.addr, port))
		}
	}
	return out
}

func interfaceHosts(allowLoopback bool) (hosts []hostCandidate, hasV6 bool) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, false
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || (iface.Flags&net.FlagLoopback != 0 && !allowLoopback) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, entry := range addrs {
			var raw net.IP
			switch a := entry.(type) {
			case *net.IPNet:
				raw = a.IP
			case *net.IPAddr:
				raw = a.IP
			}
			ip, ok := netip.AddrFromSlice(raw)
			if !ok || !usableAddr(ip, allowLoopback) {
				continue
			}
			ip = ip.Unmap()
			hosts = append(hosts, hostCandidate{addr: ip, pointToPoint: iface.Flags&net.FlagPointToPoint != 0})
			if ip.Is6() {
				hasV6 = true
			}
		}
	}
	return hosts, hasV6
}
