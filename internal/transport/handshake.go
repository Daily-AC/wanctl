package transport

import (
	"context"
	"crypto/tls"
	"net"
)

// ClientHandshake runs the mutually-authenticated TLS handshake as the client
// over an already-established net.Conn (e.g. a relayed WebSocket), then applies
// TOFU pinning against the known_servers store keyed by serverName.
func ClientHandshake(ctx context.Context, nc net.Conn, serverName string, id *Identity, known *Store) (*DialResult, error) {
	conn := tls.Client(nc, &tls.Config{
		Certificates:       []tls.Certificate{id.Cert},
		InsecureSkipVerify: true, // pinned by fingerprint, not CA
		MinVersion:         tls.VersionTLS13,
		ServerName:         "wanctl",
	})
	if err := conn.HandshakeContext(ctx); err != nil {
		nc.Close()
		return nil, err
	}
	fp, err := PeerFingerprint(conn)
	if err != nil {
		conn.Close()
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
	}
	if known.Has(fp) {
		known.Touch(fp)
		return res, nil
	}
	if err := known.Add(fp, serverName); err != nil {
		conn.Close()
		return nil, err
	}
	res.FirstSeen = true
	return res, nil
}

// ServerHandshake runs the mutually-authenticated TLS handshake as the server
// over an already-established net.Conn, returning the conn and the peer's
// fingerprint. Trust/authorization is the caller's responsibility.
func ServerHandshake(ctx context.Context, nc net.Conn, id *Identity) (*tls.Conn, string, error) {
	conn := tls.Server(nc, ServerTLSConfig(id))
	if err := conn.HandshakeContext(ctx); err != nil {
		nc.Close()
		return nil, "", err
	}
	fp, err := PeerFingerprint(conn)
	if err != nil {
		conn.Close()
		return nil, "", err
	}
	return conn, fp, nil
}
