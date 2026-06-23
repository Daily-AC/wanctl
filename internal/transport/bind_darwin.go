//go:build darwin

package transport

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

// bindSocketToIface pins the socket to interface index idx using IP_BOUND_IF
// (IPv4) or IPV6_BOUND_IF (IPv6), forcing egress out that interface regardless
// of the routing table — the key to bypassing a hijacking VPN/proxy TUN.
func bindSocketToIface(c syscall.RawConn, ip net.IP, idx int) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		if ip.To4() != nil {
			serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, idx)
		} else {
			serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_BOUND_IF, idx)
		}
	})
	if err != nil {
		return err
	}
	return serr
}
