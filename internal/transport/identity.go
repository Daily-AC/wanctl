// Package transport handles the cryptographic identity of a lanctl node and the
// mutually-authenticated TLS connection between client and server.
//
// Every node has a long-lived Ed25519 key pair wrapped in a self-signed
// certificate. A node's "fingerprint" is the SHA-256 of its certificate, shown
// in the familiar "SHA256:<base64>" form. Controllers explicitly confirm an
// unknown server fingerprint before recording it; afterwards a mismatch is a
// hard error. Devices separately authorize controller fingerprints.
package transport

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Identity is this node's persistent key pair and certificate.
type Identity struct {
	Cert        tls.Certificate
	Leaf        *x509.Certificate
	Fingerprint string // "SHA256:<base64>"
}

// ConfigDir returns the per-user directory where wanctl stores its identity and
// trust files, creating it if needed. WANCTL_CONFIG_DIR overrides the location
// (used for test isolation and for running multiple roles on one machine).
func ConfigDir() (string, error) {
	if override := os.Getenv("WANCTL_CONFIG_DIR"); override != "" {
		if err := os.MkdirAll(override, 0o700); err != nil {
			return "", err
		}
		return override, nil
	}
	base, err := os.UserConfigDir()
	if err == nil {
		dir := filepath.Join(base, "wanctl")
		if mkErr := os.MkdirAll(dir, 0o700); mkErr == nil {
			return dir, nil
		} else if runtime.GOOS != "android" {
			return "", mkErr
		}
	} else if runtime.GOOS != "android" {
		return "", err
	}
	if dir := androidConfigDir(os.Getenv, os.Executable, os.MkdirAll); dir != "" {
		return dir, nil
	}
	return "", fmt.Errorf("no writable config directory on android; set WANCTL_CONFIG_DIR")
}

// androidConfigDir finds somewhere writable to keep the node identity on
// Android, where $HOME is not dependable.
//
// Under Termux, HOME is a real writable home directory and this is never
// reached. Run the same binary from an adb shell — the way you get an agent
// onto a device without installing anything — and HOME is "/", so
// os.UserConfigDir() hands back "/.config" and creating it fails with
// "read-only file system", which killed the agent before it could even generate
// its key pair.
//
// The candidates are the writable-and-executable places Android actually has:
// TMPDIR (Termux and some shells export it), the directory the binary itself
// was put in (whoever pushed it there could write there), and /data/local/tmp,
// which is the one location the adb shell user can always write.
func androidConfigDir(getenv func(string) string, executable func() (string, error), mkdirAll func(string, os.FileMode) error) string {
	var candidates []string
	if tmp := strings.TrimSpace(getenv("TMPDIR")); tmp != "" {
		candidates = append(candidates, filepath.Join(tmp, "wanctl"))
	}
	if self, err := executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(self), ".wanctl"))
	}
	candidates = append(candidates, "/data/local/tmp/.wanctl")
	for _, dir := range candidates {
		if err := mkdirAll(dir, 0o700); err == nil {
			return dir
		}
	}
	return ""
}

// Fingerprint computes the canonical fingerprint of a DER certificate.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "SHA256:" + base64.StdEncoding.EncodeToString(sum[:])
}

// LoadOrCreateIdentity loads the node identity from the config dir, generating a
// fresh key pair and self-signed certificate on first run.
func LoadOrCreateIdentity() (*Identity, error) {
	dir, err := ConfigDir()
	if err != nil {
		return nil, err
	}
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")

	if _, err := os.Stat(certPath); err == nil {
		return loadIdentity(certPath, keyPath)
	}
	return createIdentity(certPath, keyPath)
}

func loadIdentity(certPath, keyPath string) (*Identity, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load identity: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	cert.Leaf = leaf
	return &Identity{Cert: cert, Leaf: leaf, Fingerprint: Fingerprint(cert.Certificate[0])}, nil
}

func createIdentity(certPath, keyPath string) (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "lanctl-node"
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "lanctl-" + host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, err
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, err
	}

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	leaf, _ := x509.ParseCertificate(der)
	cert.Leaf = leaf
	return &Identity{Cert: cert, Leaf: leaf, Fingerprint: Fingerprint(der)}, nil
}

// IdentityFromSeed builds a node identity from a deterministic 32-byte Ed25519
// seed instead of generating a fresh one. Used by the remote MCP server to
// derive a stable controller identity per namespace via HKDF(server_secret,
// namespace) — no private key ever touches the database, the fingerprint is
// stable across reconnects, and the user pairs once per device rather than
// per session. The certificate's NotBefore/NotAfter are fixed so the resulting
// DER (and thus the fingerprint) is fully deterministic for the same seed.
func IdentityFromSeed(seed []byte, commonName string) (*Identity, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("seed must be %d bytes, got %d", ed25519.SeedSize, len(seed))
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)

	serial := new(big.Int).SetBytes(seed[:16])
	if serial.Sign() == 0 {
		serial.SetInt64(1)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:     time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	leaf, _ := x509.ParseCertificate(der)
	cert.Leaf = leaf
	return &Identity{Cert: cert, Leaf: leaf, Fingerprint: Fingerprint(der)}, nil
}

// ShortFingerprint returns a human-comparable abbreviation for prompts.
func ShortFingerprint(fp string) string {
	if len(fp) <= 25 {
		return fp
	}
	return fp[:14] + "…" + fp[len(fp)-6:]
}
