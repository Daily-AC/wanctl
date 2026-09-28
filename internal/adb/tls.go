package adb

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
)

// TLSConfigFunc supplies the client configuration for an STLS upgrade. It is a
// function rather than a value because building it means loading the paired
// key material, which is wasted work on a connection that never needs TLS.
type TLSConfigFunc func() (*tls.Config, error)

// ErrKeyRejected means an adbd that demands TLS refused this agent's key. adbd
// is running and reachable; what is missing is the pairing, so a caller should
// not suggest turning wireless debugging on.
var ErrKeyRejected = errors.New("adbd does not accept wanctl's key")

// startTLS answers adbd's STLS and upgrades the connection.
//
// Android 11+ wireless debugging always takes this path. The device
// authenticates the client by its certificate, which must carry the key it
// recorded during pairing. adbd speaks TLS 1.3 only, and in 1.3 the client
// finishes its half of the handshake before the server has judged the client
// certificate — so an unpaired key does not fail here. adbd's refusal arrives
// as an alert on the first read afterwards, and handshake() explains it exactly
// as an alert here would be explained. (AOSP's own client turns on a
// post-handshake check for the same reason.)
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
		if isPeerAlert(err) {
			return keyRejected(err)
		}
		return fmt.Errorf("adb: TLS handshake with adbd: %w", err)
	}
	c.c = tc
	return nil
}

// keyRejected explains adbd refusing the handshake with an alert. On a phone
// where the channel has worked before, that means the pairing is gone, and
// nothing in a TLS alert says so. AOSP's AdbDebuggingManager forgets a key
// that has not connected for seven days (DEFAULT_ADB_ALLOWED_CONNECTION_TIME)
// unless the owner turned that timeout off, and Revoke USB debugging
// authorizations forgets every key at once, the paired ones included.
func keyRejected(err error) error {
	return fmt.Errorf("adb: TLS handshake with adbd: %w (%w: this device never paired it, or the pairing lapsed — "+
		"Android revokes a pairing that has not connected for 7 days unless Developer options → "+
		"Disable adb authorization timeout is on, and Revoke USB debugging authorizations removes every pairing; "+
		"pair again from the portal, Device settings → ADB pairing)", err, ErrKeyRejected)
}

// isPeerAlert reports whether err is a TLS alert the other side sent, which
// crypto/tls reports as a net.OpError whose Op is "remote error".
func isPeerAlert(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "remote error"
}

// stlsVersion is the STLS protocol version adbd expects in arg0.
const stlsVersion = 0x01000000
