package direct

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"math/big"
	"time"

	"github.com/quic-go/quic-go"
)

const directALPN = "wanctl-direct/1"

func newCertificate() (tls.Certificate, string, error) {
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, private)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, "", err
	}
	hash := sha256.Sum256(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private, Leaf: leaf}, base64.RawURLEncoding.EncodeToString(hash[:]), nil
}

func verifyPeer(state tls.ConnectionState, pin string) error {
	if state.NegotiatedProtocol != directALPN {
		return errors.New("direct: wrong ALPN")
	}
	if len(state.PeerCertificates) != 1 {
		return errors.New("direct: expected one peer certificate")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(pin)
	if err != nil || len(decoded) != sha256.Size {
		return errors.New("direct: invalid peer certificate pin")
	}
	hash := sha256.Sum256(state.PeerCertificates[0].Raw)
	if !equalPins(hash[:], decoded) {
		return errors.New("direct: peer certificate pin mismatch")
	}
	return nil
}

func equalPins(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

func verifyTLSConfig(cert tls.Certificate, peerPin string, server bool) *tls.Config {
	conf := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, NextProtos: []string{directALPN}, InsecureSkipVerify: true, VerifyConnection: func(state tls.ConnectionState) error { return verifyPeer(state, peerPin) }}
	if server {
		conf.ClientAuth = tls.RequireAnyClientCert
		conf.InsecureSkipVerify = false
	}
	return conf
}

func quicConfig() *quic.Config {
	return &quic.Config{KeepAlivePeriod: 5 * time.Second, MaxIdleTimeout: 15 * time.Second, MaxIncomingStreams: 1, MaxIncomingUniStreams: -1, Allow0RTT: false}
}
