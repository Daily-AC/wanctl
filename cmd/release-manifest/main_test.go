package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/xml"
	"math/big"
	"testing"
)

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	// 2048 keeps the test fast; rsaSigningKey's floor is 2048 so this also
	// exercises the boundary being accepted rather than rejected.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// TestRSASigningKeyAcceptsBothDEREncodings covers a real stumble: the docs told
// the operator to generate the key with `openssl genpkey -outform DER`, which
// emits PKCS#1 for RSA, while the loader only accepted PKCS#8. Following the
// instructions produced a key the release build refused.
func TestRSASigningKeyAcceptsBothDEREncodings(t *testing.T) {
	key := testKey(t)
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	for name, der := range map[string][]byte{
		"pkcs8": pkcs8,
		"pkcs1": x509.MarshalPKCS1PrivateKey(key),
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("WANCTL_RELEASE_RSA_KEY", base64.StdEncoding.EncodeToString(der))
			loaded, err := rsaSigningKey()
			if err != nil {
				t.Fatalf("rsaSigningKey: %v", err)
			}
			if loaded.N.Cmp(key.N) != 0 {
				t.Fatal("loaded a different key than was encoded")
			}
		})
	}
}

func TestRSASigningKeyRejectsBadInput(t *testing.T) {
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"empty":      "",
		"not base64": "!!!!",
		"not a key":  base64.StdEncoding.EncodeToString([]byte("neither pkcs1 nor pkcs8")),
		"undersized": base64.StdEncoding.EncodeToString(x509.MarshalPKCS1PrivateKey(weak)),
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("WANCTL_RELEASE_RSA_KEY", value)
			if _, err := rsaSigningKey(); err == nil {
				t.Fatal("expected an error, got none")
			}
		})
	}
}

// TestRSAPublicKeyXML checks the shape PowerShell 5.1's FromXmlString requires:
// well-formed XML whose Modulus is the big-endian modulus with no leading zero
// padding. A stray leading zero byte is accepted by some parsers and produces a
// key that silently fails to verify, so assert the exact bytes.
func TestRSAPublicKeyXML(t *testing.T) {
	key := testKey(t)
	var parsed struct {
		Modulus  string `xml:"Modulus"`
		Exponent string `xml:"Exponent"`
	}
	if err := xml.Unmarshal([]byte(rsaPublicKeyXML(&key.PublicKey)), &parsed); err != nil {
		t.Fatalf("not well-formed XML: %v", err)
	}
	modulus, err := base64.StdEncoding.DecodeString(parsed.Modulus)
	if err != nil {
		t.Fatal(err)
	}
	if new(big.Int).SetBytes(modulus).Cmp(key.N) != 0 {
		t.Fatal("modulus does not round-trip")
	}
	if modulus[0] == 0 {
		t.Fatal("modulus has a leading zero byte; FromXmlString wants it stripped")
	}
	exponent, err := base64.StdEncoding.DecodeString(parsed.Exponent)
	if err != nil {
		t.Fatal(err)
	}
	if got := int(new(big.Int).SetBytes(exponent).Int64()); got != key.E {
		t.Fatalf("exponent = %d, want %d", got, key.E)
	}
}
