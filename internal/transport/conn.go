package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"
)

// ServerTLSConfig builds the listener config. We require the client to present a
// certificate but verify it ourselves (by fingerprint), so the standard CA
// verification is disabled.
func ServerTLSConfig(id *Identity) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{id.Cert},
		ClientAuth:   tls.RequireAnyClientCert,
		MinVersion:   tls.VersionTLS13,
	}
}

// PeerFingerprint returns the fingerprint of the connection's peer certificate.
func PeerFingerprint(c *tls.Conn) (string, error) {
	st := c.ConnectionState()
	if len(st.PeerCertificates) == 0 {
		return "", fmt.Errorf("peer presented no certificate")
	}
	return Fingerprint(st.PeerCertificates[0].Raw), nil
}

// MismatchError signals that a pinned server presented a different identity.
type MismatchError struct {
	Name    string
	Stored  string
	Offered string
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("server %q identity changed!\n  pinned:  %s\n  offered: %s\nrefusing to connect (possible impersonation). To re-pin after independently verifying the fingerprint, run:\n  wanctl trust server --target %q --fingerprint %q --replace",
		e.Name, e.Stored, e.Offered, e.Name, e.Offered)
}

// DialResult carries the established connection plus whether this was a first
// (TOFU) pairing with the server.
type DialResult struct {
	Conn      *tls.Conn
	PeerFP    string
	FirstSeen bool
}

// RawDial establishes a mutually-authenticated TLS connection without any trust
// pinning. It is used for discovery probes, where triggering a pairing prompt or
// persisting trust would be inappropriate.
func RawDial(ctx context.Context, addr string, id *Identity) (*tls.Conn, string, error) {
	d := &net.Dialer{Timeout: 8 * time.Second, Control: bindControl}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, "", err
	}
	conn := tls.Client(raw, &tls.Config{
		Certificates:       []tls.Certificate{id.Cert},
		InsecureSkipVerify: true, // we pin by fingerprint, not CA
		MinVersion:         tls.VersionTLS13,
		ServerName:         "lanctl",
	})
	if err := conn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, "", err
	}
	fp, err := PeerFingerprint(conn)
	if err != nil {
		conn.Close()
		return nil, "", err
	}
	return conn, fp, nil
}

// Dial connects to a lanctl server at addr, completes the mutually-authenticated
// handshake, and checks the client's known_servers store.
// serverName is the logical name used to pin identity across IP changes.
func Dial(ctx context.Context, addr, serverName string, id *Identity, known *Store) (*DialResult, error) {
	conn, fp, err := RawDial(ctx, addr, id)
	if err != nil {
		return nil, err
	}

	res := &DialResult{Conn: conn, PeerFP: fp}
	if serverName != "" {
		if pinned, ok := known.GetByName(serverName); ok {
			if pinned.Fingerprint != fp {
				conn.Close()
				return nil, &MismatchError{Name: serverName, Stored: pinned.Fingerprint, Offered: fp}
			}
			known.Touch(fp)
			return res, nil
		}
		res.FirstSeen = true
		return res, nil
	}
	if known.Has(fp) {
		known.Touch(fp)
		return res, nil
	}
	res.FirstSeen = true
	return res, nil
}
