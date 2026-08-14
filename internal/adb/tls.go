package adb

import (
	"crypto/tls"
	"fmt"
)

// TLSConfigFunc supplies the client configuration for an STLS upgrade. It is a
// function rather than a value because building it means loading the paired
// key material, which is wasted work on a connection that never needs TLS.
type TLSConfigFunc func() (*tls.Config, error)

// startTLS answers adbd's STLS and upgrades the connection.
//
// Android 11+ wireless debugging always takes this path. The device
// authenticates the client by its certificate, which must carry the key it
// recorded during pairing — so an unpaired key fails here, at the handshake,
// rather than later with a confusing protocol error.
//
// InsecureSkipVerify is set, and that is not a shortcut: adbd presents a
// self-signed certificate with no name that any CA has heard of, and the
// connection is to 127.0.0.1 on the same device this process is already running
// on. There is no third party in the path to authenticate. What actually gates
// access is the reverse direction — the device verifying our certificate
// against its own paired-keys list.
func (c *Conn) startTLS(configure TLSConfigFunc) error {
	cfg, err := configure()
	if err != nil {
		return fmt.Errorf("adb: build TLS client config: %w", err)
	}
	if err := c.send(message{Command: cmdStls, Arg0: stlsVersion}); err != nil {
		return err
	}
	tc := tls.Client(c.c, cfg)
	if err := tc.Handshake(); err != nil {
		return fmt.Errorf("adb: TLS handshake with adbd: %w "+
			"(this usually means the device has not paired this key)", err)
	}
	c.c = tc
	return nil
}

// stlsVersion is the STLS protocol version adbd expects in arg0.
const stlsVersion = 0x01000000
