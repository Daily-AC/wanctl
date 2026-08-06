//go:build android

package androiddns

import (
	"net"
	"os"
)

// init replaces the process-wide resolver before anything dials. main imports
// this package for its side effect only; every role (agent, controller) needs
// it, because on Android the stock resolver cannot resolve anything at all.
func init() {
	if r := Resolver(Nameservers(os.Getenv, os.ReadFile)); r != nil {
		net.DefaultResolver = r
	}
}
