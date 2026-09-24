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

func usableAddr(ip netip.Addr, allowLoopback bool) bool {
	ip = ip.Unmap()
	return ip.IsValid() && ip.IsGlobalUnicast() && (allowLoopback || !ip.IsLoopback()) && !ip.IsLinkLocalUnicast() && ip != netip.MustParseAddr("255.255.255.255")
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
	return orderCandidatePairs(pairs, reflexive, hosts, port, allowLoopback)
}

func orderCandidatePairs(defaults, reflexive []netip.AddrPort, hosts []hostCandidate, port uint16, allowLoopback bool) []string {
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
