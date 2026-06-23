package transport

import (
	"net"
	"syscall"
)

// LAN connections must reach a host on a directly-connected subnet. When a VPN
// or proxy TUN (e.g. Clash/mihomo on 198.18.0.0/15) steals the default route,
// the kernel's route lookup sends our LAN dial into the tunnel and it fails with
// "no route to host". Source-address binding doesn't help because the route
// lookup still picks the tunnel.
//
// The fix is to pin the socket to the physical interface that actually owns the
// destination's subnet, bypassing the routing table entirely (IP_BOUND_IF on
// macOS). This is transparent to the user — no proxy reconfiguration needed.

// bindControl is a net.Dialer.Control hook that binds the socket to the physical
// interface whose subnet contains the destination IP. If no such interface is
// found (destination not on a local subnet), it leaves routing untouched.
func bindControl(network, address string, c syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil
	}
	idx, ok := interfaceForIP(ip)
	if !ok {
		return nil
	}
	return bindSocketToIface(c, ip, idx)
}

// interfaceForIP returns the index of the up, non-loopback interface whose
// directly-connected subnet contains ip. A TUN with an unrelated /30 (like
// 198.18.0.0/30) is naturally excluded because it doesn't contain a LAN IP.
func interfaceForIP(ip net.IP) (int, bool) {
	if ip == nil {
		return 0, false
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return 0, false
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.Contains(ip) {
				return ifc.Index, true
			}
		}
	}
	return 0, false
}
