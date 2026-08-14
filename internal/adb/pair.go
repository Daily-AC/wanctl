package adb

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"time"

	"golang.org/x/crypto/hkdf"
)

// Pairing is Android 11+ wireless-debugging pairing: the exchange that puts
// this agent's public key into the device's adb_keys so it can connect at all.
//
// It is what makes the adb channel usable on a phone that is neither rooted nor
// attached to a computer — its owner turns on Developer options → Wireless
// debugging → Pair device with pairing code, and reads six digits off the
// screen.
//
// The shape, from AOSP's packages/modules/adb/pairing_connection:
//
//  1. TCP to the pairing port (announced over mDNS as _adb-tls-pairing._tcp).
//  2. TLS, with both sides presenting self-signed certificates that neither
//     verifies. TLS is not providing authentication here — it provides a
//     channel to bind to.
//  3. That binding is the point: 64 bytes of exported keying material are
//     APPENDED to the pairing code before SPAKE2 sees it, so a man in the
//     middle who relays the PAKE cannot steal the resulting session. Getting
//     the code right but the binding wrong yields a key mismatch, not a
//     silent downgrade.
//  4. SPAKE2 over the combined password (see spake2.go).
//  5. Each side sends its PeerInfo — for us, an adb public key — encrypted
//     with AES-128-GCM under a key derived from the SPAKE2 output.
const (
	pairingHeaderSize    = 6
	pairingHeaderVersion = 1

	pairingSPAKE2Msg = 0 // PairingPacket.Type.SPAKE2_MSG
	pairingPeerInfo  = 1 // PairingPacket.Type.PEER_INFO

	// peerInfoSize is fixed: adbd reads a whole struct and rejects anything
	// else, so the RSA key is padded out to it rather than sent as it is.
	peerInfoSize = 8192
	// adbRSAPublicKey is PeerInfoType::ADB_RSA_PUB_KEY.
	adbRSAPublicKey = 0

	// maxPairingPayload bounds what a peer can make this side allocate.
	maxPairingPayload = 16384

	// exportedKeySize is how much keying material AOSP mixes into the password.
	exportedKeySize = 64
)

// exportLabel is the TLS exporter label — with its trailing NUL.
//
// AOSP passes `sizeof(kExportedKeyLabel)` as the label length, and sizeof on a
// char array counts the terminator, so the label adbd actually uses is ten
// bytes: "adb-label\0". Dropping that NUL produces a perfectly valid exporter
// output that simply does not match the device's, and the only symptom is that
// pairing fails.
const exportLabel = "adb-label\x00"

// hkdfInfo is the context string AOSP derives its AES key under. Note that
// here AOSP passes `sizeof(info) - 1`, so this one does NOT carry a NUL — the
// same file is inconsistent about it, which is why each string is pinned
// against the source rather than assumed.
const hkdfInfo = "adb pairing_auth aes-128-gcm key"

// The SPAKE2 identity strings, WITH their trailing NULs.
//
// AOSP declares them as `static const uint8_t kClientName[] = "adb pair
// client"` and passes `sizeof(kClientName)` as the length, which counts the
// terminator. The names go into the SPAKE2 transcript along with their
// lengths, so dropping the NUL yields a different key on every pairing attempt
// — and the only symptom is the final decrypt failing, with the pairing code
// as the obvious-but-wrong suspect. Verified against a real device on
// 2026-08-14: without the NUL, pairing reached the last step and failed.
const (
	spakeClientName = "adb pair client\x00"
	spakeServerName = "adb pair server\x00"
)

// Pair performs the pairing exchange against addr and, on success, leaves this
// agent's key registered on the device.
//
// code is the six-digit pairing code shown on the device.
func Pair(ctx context.Context, addr string, key *Key, code string) error {
	cert, err := selfSignedCert()
	if err != nil {
		return err
	}
	var d net.Dialer
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("adb pair: dial %s: %w", addr, err)
	}
	defer raw.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(deadline)
	}

	conn := tls.Client(raw, &tls.Config{
		Certificates: []tls.Certificate{cert},
		// adbd presents a self-signed certificate with no name any CA has
		// heard of, and AOSP's own client accepts any peer certificate here.
		// Authentication comes from SPAKE2 over a password bound to this very
		// TLS session (see ExportKeyingMaterial below), not from the PKI.
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS13,
	})
	if err := conn.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("adb pair: TLS handshake: %w", err)
	}

	state := conn.ConnectionState()
	material, err := state.ExportKeyingMaterial(exportLabel, nil, exportedKeySize)
	if err != nil {
		return fmt.Errorf("adb pair: export keying material: %w", err)
	}
	password := append([]byte(code), material...)

	s := newSPAKE2(spake2Alice, spakeClientName, spakeServerName)
	myMsg, err := s.generateMsg(password)
	if err != nil {
		return err
	}
	if err := writePairingPacket(conn, pairingSPAKE2Msg, myMsg); err != nil {
		return fmt.Errorf("adb pair: send SPAKE2 message: %w", err)
	}
	typ, theirMsg, err := readPairingPacket(conn)
	if err != nil {
		return fmt.Errorf("adb pair: read SPAKE2 message: %w", err)
	}
	if typ != pairingSPAKE2Msg {
		return fmt.Errorf("adb pair: expected a SPAKE2 message, got packet type %d", typ)
	}
	keyMaterial, err := s.processMsg(theirMsg)
	if err != nil {
		return fmt.Errorf("adb pair: %w", err)
	}

	cipher, err := newPairingCipher(keyMaterial)
	if err != nil {
		return err
	}
	sealed, err := cipher.seal(peerInfoPayload(key))
	if err != nil {
		return err
	}
	if err := writePairingPacket(conn, pairingPeerInfo, sealed); err != nil {
		return fmt.Errorf("adb pair: send peer info: %w", err)
	}

	typ, encrypted, err := readPairingPacket(conn)
	if err != nil {
		// A wrong pairing code shows up here: the device's PeerInfo is
		// encrypted under a key we did not derive, and it usually just hangs
		// up. Name the likely cause instead of reporting a bare EOF.
		return fmt.Errorf("adb pair: no peer info from the device (%w); "+
			"this is what a wrong pairing code looks like", err)
	}
	if typ != pairingPeerInfo {
		return fmt.Errorf("adb pair: expected peer info, got packet type %d", typ)
	}
	if _, err := cipher.open(encrypted); err != nil {
		return fmt.Errorf("adb pair: could not decrypt the device's peer info (%w); "+
			"the pairing code was probably wrong", err)
	}
	return nil
}

// peerInfoPayload builds the fixed-size struct adbd expects: a type byte
// followed by the payload, padded out to 8192 bytes.
func peerInfoPayload(key *Key) []byte {
	buf := make([]byte, peerInfoSize)
	buf[0] = adbRSAPublicKey
	copy(buf[1:], key.PublicKey())
	return buf
}

// pairingCipher is AES-128-GCM with the sequence-number nonce discipline AOSP
// uses: a counter that starts at zero and increments per message, kept
// separately for each direction.
type pairingCipher struct {
	aead interface {
		Seal(dst, nonce, plaintext, additionalData []byte) []byte
		Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
		NonceSize() int
	}
	encSeq uint64
	decSeq uint64
}

func newPairingCipher(keyMaterial []byte) (*pairingCipher, error) {
	k := make([]byte, 16)
	if _, err := io.ReadFull(hkdf.New(sha256.New, keyMaterial, nil, []byte(hkdfInfo)), k); err != nil {
		return nil, err
	}
	aead, err := newAESGCM(k)
	if err != nil {
		return nil, err
	}
	return &pairingCipher{aead: aead}, nil
}

func (c *pairingCipher) nonce(seq uint64) []byte {
	n := make([]byte, c.aead.NonceSize())
	// AOSP copies the counter into a zeroed nonce buffer, little-endian, and
	// leaves the remaining bytes zero.
	binary.LittleEndian.PutUint64(n, seq)
	return n
}

func (c *pairingCipher) seal(plaintext []byte) ([]byte, error) {
	out := c.aead.Seal(nil, c.nonce(c.encSeq), plaintext, nil)
	c.encSeq++
	return out, nil
}

func (c *pairingCipher) open(ciphertext []byte) ([]byte, error) {
	out, err := c.aead.Open(nil, c.nonce(c.decSeq), ciphertext, nil)
	if err != nil {
		return nil, err
	}
	c.decSeq++
	return out, nil
}

// writePairingPacket frames a payload: version, type, then a BIG-endian length.
// (The SPAKE2 transcript uses little-endian lengths; these are different layers
// and genuinely disagree.)
func writePairingPacket(w io.Writer, typ byte, payload []byte) error {
	var hdr [pairingHeaderSize]byte
	hdr[0] = pairingHeaderVersion
	hdr[1] = typ
	binary.BigEndian.PutUint32(hdr[2:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func readPairingPacket(r io.Reader) (byte, []byte, error) {
	var hdr [pairingHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	if hdr[0] != pairingHeaderVersion {
		return 0, nil, fmt.Errorf("adb pair: unsupported pairing header version %d", hdr[0])
	}
	size := binary.BigEndian.Uint32(hdr[2:])
	if size > maxPairingPayload {
		return 0, nil, fmt.Errorf("adb pair: payload of %d bytes exceeds the %d-byte limit", size, maxPairingPayload)
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[1], payload, nil
}

// selfSignedCert builds the throwaway certificate this side presents. Nothing
// verifies it; it exists because TLS requires a client certificate here and
// because its session is what the password is bound to.
func selfSignedCert() (tls.Certificate, error) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "wanctl adb pairing"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, nil
}

var errShortKey = errors.New("adb: AES key must be 16 bytes")

// newAESGCM wraps the standard AEAD so the cipher field above stays an
// interface (which keeps pairingCipher testable without a real key).
func newAESGCM(key []byte) (interface {
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
	Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
	NonceSize() int
}, error) {
	if len(key) != 16 {
		return nil, errShortKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
