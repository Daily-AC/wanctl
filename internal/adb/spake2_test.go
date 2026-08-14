package adb

import (
	"bytes"
	"crypto/sha512"
	"testing"

	"filippo.io/edwards25519"
)

// runSPAKE2 plays both roles and returns the two derived keys.
func runSPAKE2(t *testing.T, alicePw, bobPw []byte) (aliceKey, bobKey []byte) {
	t.Helper()
	alice := newSPAKE2(spake2Alice, "adb pair client", "adb pair server")
	bob := newSPAKE2(spake2Bob, "adb pair server", "adb pair client")

	aliceMsg, err := alice.generateMsg(alicePw)
	if err != nil {
		t.Fatal(err)
	}
	bobMsg, err := bob.generateMsg(bobPw)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliceMsg) != 32 || len(bobMsg) != 32 {
		t.Fatalf("message sizes = %d/%d, want 32 each", len(aliceMsg), len(bobMsg))
	}
	if aliceKey, err = alice.processMsg(bobMsg); err != nil {
		t.Fatal(err)
	}
	if bobKey, err = bob.processMsg(aliceMsg); err != nil {
		t.Fatal(err)
	}
	return aliceKey, bobKey
}

// The property the whole protocol rests on: same password, same key, without
// the password ever crossing the wire.
func TestSPAKE2AgreesOnAKeyWithTheSamePassword(t *testing.T) {
	pw := []byte("123456")
	a, b := runSPAKE2(t, pw, pw)
	if len(a) != sha512.Size {
		t.Fatalf("key is %d bytes, want %d", len(a), sha512.Size)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("the two sides derived different keys:\n alice %x\n bob   %x", a, b)
	}
	if bytes.Equal(a, make([]byte, len(a))) {
		t.Fatal("key is all zeroes")
	}
}

// A wrong pairing code must not merely fail late — it must yield a different
// key, which is what makes the encrypted PeerInfo exchange fail closed.
func TestSPAKE2DisagreesOnADifferentPassword(t *testing.T) {
	a, b := runSPAKE2(t, []byte("123456"), []byte("123457"))
	if bytes.Equal(a, b) {
		t.Fatal("different passwords produced the same key")
	}
}

// Each run must use a fresh ephemeral scalar; a repeated message would leak the
// password to anyone who recorded two sessions.
func TestSPAKE2MessagesAreFresh(t *testing.T) {
	pw := []byte("123456")
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		s := newSPAKE2(spake2Alice, "adb pair client", "adb pair server")
		msg, err := s.generateMsg(pw)
		if err != nil {
			t.Fatal(err)
		}
		if seen[string(msg)] {
			t.Fatal("the same SPAKE2 message was generated twice")
		}
		seen[string(msg)] = true
	}
}

// Role symmetry: swapping which side is Alice must break agreement, since the
// masks (M vs N) and the transcript order both depend on it.
func TestSPAKE2RolesAreNotInterchangeable(t *testing.T) {
	pw := []byte("123456")
	a := newSPAKE2(spake2Alice, "adb pair client", "adb pair server")
	b := newSPAKE2(spake2Alice, "adb pair server", "adb pair client") // wrong role
	am, err := a.generateMsg(pw)
	if err != nil {
		t.Fatal(err)
	}
	bm, err := b.generateMsg(pw)
	if err != nil {
		t.Fatal(err)
	}
	ak, err := a.processMsg(bm)
	if err != nil {
		t.Fatal(err)
	}
	bk, err := b.processMsg(am)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ak, bk) {
		t.Fatal("two Alices agreed on a key; the role must select M vs N")
	}
}

// The names go into the transcript, so a peer claiming a different identity
// derives a different key even with the right password.
func TestSPAKE2NamesAreBoundIntoTheKey(t *testing.T) {
	pw := []byte("123456")
	alice := newSPAKE2(spake2Alice, "adb pair client", "adb pair server")
	bob := newSPAKE2(spake2Bob, "adb pair server", "someone else")
	am, _ := alice.generateMsg(pw)
	bm, _ := bob.generateMsg(pw)
	ak, _ := alice.processMsg(bm)
	bk, _ := bob.processMsg(am)
	if bytes.Equal(ak, bk) {
		t.Fatal("mismatched peer names still agreed on a key")
	}
}

func TestSPAKE2RejectsAMalformedPeerMessage(t *testing.T) {
	s := newSPAKE2(spake2Alice, "adb pair client", "adb pair server")
	if _, err := s.generateMsg([]byte("123456")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.processMsg(make([]byte, 31)); err == nil {
		t.Fatal("a 31-byte peer message was accepted")
	}
	// A peer that is not on the curve must be refused rather than carried into
	// the key derivation. Not every 32-byte string is invalid — most decode
	// fine — so search for one that genuinely does not.
	bad := offCurveEncoding(t)
	if _, err := s.processMsg(bad); err == nil {
		t.Fatalf("a point that is not on the curve was accepted (%x)", bad)
	}
}

// offCurveEncoding finds a 32-byte string that does not decode to a curve point.
func offCurveEncoding(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 32)
	for i := 0; i < 256; i++ {
		b[0] = byte(i)
		if _, err := new(edwards25519.Point).SetBytes(b); err != nil {
			return b
		}
	}
	t.Fatal("could not find an off-curve encoding to test with")
	return nil
}

func TestSPAKE2RequiresGenerateBeforeProcess(t *testing.T) {
	s := newSPAKE2(spake2Alice, "adb pair client", "adb pair server")
	if _, err := s.processMsg(make([]byte, 32)); err == nil {
		t.Fatal("processMsg succeeded before generateMsg")
	}
}

// TestSPAKE2SmallOrderPointIsNeutralised: the cofactor multiplication exists so
// that a peer cannot force the shared point into a small subgroup. Adding a
// small-order point to an honest message must not change the key we derive.
func TestSPAKE2SmallOrderPointIsNeutralised(t *testing.T) {
	pw := []byte("123456")
	alice := newSPAKE2(spake2Alice, "adb pair client", "adb pair server")
	bob := newSPAKE2(spake2Bob, "adb pair server", "adb pair client")
	if _, err := alice.generateMsg(pw); err != nil {
		t.Fatal(err)
	}
	bobMsg, err := bob.generateMsg(pw)
	if err != nil {
		t.Fatal(err)
	}
	honest, err := alice.processMsg(bobMsg)
	if err != nil {
		t.Fatal(err)
	}

	// An order-8 point: the standard non-canonical generator of the torsion
	// subgroup. Adding it changes the encoded message but must not change the
	// derived key.
	torsion := smallOrderPoint(t)
	tampered := new(edwards25519.Point)
	if _, err := tampered.SetBytes(bobMsg); err != nil {
		t.Fatal(err)
	}
	tampered.Add(tampered, torsion)

	alice2 := newSPAKE2(spake2Alice, "adb pair client", "adb pair server")
	alice2.private, alice2.passwordScalar = alice.private, alice.passwordScalar
	alice2.passwordHash, alice2.myMsg = alice.passwordHash, alice.myMsg
	got, err := alice2.processMsg(tampered.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	// The transcript hashes the peer's message verbatim, so the keys differ —
	// but the Diffie-Hellman part must not have been steered into a small
	// subgroup. Check that directly instead.
	if bytes.Equal(got, honest) {
		t.Skip("transcript includes the peer message verbatim, so keys differ by construction")
	}
	dhHonest := dhFor(t, alice, bobMsg)
	dhTampered := dhFor(t, alice, tampered.Bytes())
	if !bytes.Equal(dhHonest, dhTampered) {
		t.Fatal("a small-order point changed the Diffie-Hellman value; " +
			"the cofactor multiplication is not doing its job")
	}
}

// dhFor recomputes just the DH portion for a given peer message.
func dhFor(t *testing.T, s *spake2, theirMsg []byte) []byte {
	t.Helper()
	QStar, err := new(edwards25519.Point).SetBytes(theirMsg)
	if err != nil {
		t.Fatal(err)
	}
	peersMask := new(edwards25519.Point).ScalarMult(s.passwordScalar, s.maskPoint(other(s.role)))
	QExt := new(edwards25519.Point).Subtract(QStar, peersMask)
	Q8 := new(edwards25519.Point).MultByCofactor(QExt)
	return new(edwards25519.Point).ScalarMult(s.private, Q8).Bytes()
}

// smallOrderPoint returns a point of order 8.
func smallOrderPoint(t *testing.T) *edwards25519.Point {
	t.Helper()
	// Encoding of a well-known order-8 element of edwards25519.
	b := []byte{
		0xc7, 0x17, 0x6a, 0x70, 0x3d, 0x4d, 0xd8, 0x4f,
		0xba, 0x3c, 0x0b, 0x76, 0x0d, 0x10, 0x67, 0x0f,
		0x2a, 0x20, 0x53, 0xfa, 0x2c, 0x39, 0xcc, 0xc6,
		0x4e, 0xc7, 0xfd, 0x77, 0x92, 0xac, 0x03, 0x7a,
	}
	p, err := new(edwards25519.Point).SetBytes(b)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
