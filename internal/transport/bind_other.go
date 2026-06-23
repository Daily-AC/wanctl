//go:build !darwin

package transport

import (
	"net"
	"syscall"
)

// bindSocketToIface is a no-op on platforms without IP_BOUND_IF. The TUN-hijack
// problem this guards against is specific to macOS proxy tools; elsewhere normal
// routing applies.
func bindSocketToIface(c syscall.RawConn, ip net.IP, idx int) error {
	return nil
}
