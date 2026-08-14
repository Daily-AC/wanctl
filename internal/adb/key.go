package adb

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
)

// keyBits is fixed at 2048 because adb's public-key encoding is: the struct it
// serializes has a fixed 256-byte modulus field and adbd rejects anything else.
const keyBits = 2048

// Key is the RSA identity this agent presents to adbd. It is a wanctl-owned key
// stored in wanctl's own config dir — deliberately not ~/.android/adbkey, which
// on a phone belongs to whatever pushed a binary over USB and is not ours to
// reuse or to overwrite.
type Key struct {
	priv *rsa.PrivateKey
	// name is what appears in the device's "Allow USB debugging?" prompt and in
	// its adb_keys file afterwards, so the owner can tell later what they let in.
	name string
}

// LoadOrCreateKey reads the agent's adb key from dir, generating it on first
// use. Generation takes a second or two on a phone, which is why it is done
// once and persisted rather than per connection.
func LoadOrCreateKey(dir, name string) (*Key, error) {
	path := filepath.Join(dir, "adbkey.pem")
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		block, _ := pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("adb: %s is not PEM", path)
		}
		priv, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("adb: parse %s: %w", path, err)
		}
		return &Key{priv: priv, name: name}, nil
	case !os.IsNotExist(err):
		return nil, err
	}

	priv, err := rsa.GenerateKey(rand.Reader, keyBits)
	if err != nil {
		return nil, err
	}
	encoded := pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv),
	})
	// 0600: this key is what authorizes shell access to the device it lives on.
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		return nil, err
	}
	return &Key{priv: priv, name: name}, nil
}

// Sign answers an AUTH token. adbd hands out 20 random bytes and expects a
// PKCS#1 v1.5 signature carrying a SHA-1 DigestInfo prefix — the token is
// already a digest-sized blob, so nothing is hashed here, which is exactly what
// adbd's RSA_verify(NID_sha1, …) checks against.
func (k *Key) Sign(token []byte) ([]byte, error) {
	return rsa.SignPKCS1v15(nil, k.priv, crypto.SHA1, token)
}

// PublicKey encodes the public key in the format adbd stores in
// /data/misc/adb/adb_keys: a fixed-layout struct, base64'd, followed by a space
// and a human-readable name.
//
// The struct is not any standard encoding — it is BoringSSL's Montgomery
// pre-computation laid out for a bootloader that has no bignum library:
//
//	uint32 modulus_size_words   (64, i.e. 2048 bits)
//	uint32 n0inv                (-1/n[0] mod 2^32)
//	uint8  modulus[256]         (little-endian)
//	uint8  rr[256]              ((2^2048)^2 mod n, little-endian)
//	uint32 exponent
func (k *Key) PublicKey() []byte {
	const modulusBytes = keyBits / 8
	n := k.priv.N

	// n0inv = 2^32 - (n mod 2^32)^-1 mod 2^32
	r32 := new(big.Int).Lsh(big.NewInt(1), 32)
	n0 := new(big.Int).Mod(n, r32)
	n0inv := new(big.Int).ModInverse(n0, r32)
	n0inv.Sub(r32, n0inv)

	// rr = (2^2048)^2 mod n
	rr := new(big.Int).Lsh(big.NewInt(1), keyBits)
	rr.Mod(rr.Mul(rr, rr), n)

	buf := make([]byte, 0, 4+4+modulusBytes+modulusBytes+4)
	buf = binary.LittleEndian.AppendUint32(buf, modulusBytes/4)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(n0inv.Uint64()))
	buf = append(buf, littleEndianPadded(n, modulusBytes)...)
	buf = append(buf, littleEndianPadded(rr, modulusBytes)...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(k.priv.E))

	out := make([]byte, base64.StdEncoding.EncodedLen(len(buf)))
	base64.StdEncoding.Encode(out, buf)
	// The trailing NUL is part of what adbd expects in the AUTH payload; the
	// name after the space is what the device shows and remembers.
	return append(append(out, ' '), append([]byte(k.name), 0)...)
}

// littleEndianPadded renders v as exactly size little-endian bytes.
func littleEndianPadded(v *big.Int, size int) []byte {
	be := v.Bytes()
	out := make([]byte, size)
	for i, b := range be {
		if i >= size {
			break
		}
		out[len(be)-1-i] = b
	}
	return out
}
