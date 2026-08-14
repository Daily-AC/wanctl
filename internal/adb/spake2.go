package adb

import (
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"filippo.io/edwards25519"
)

// This is BoringSSL's SPAKE2 over edwards25519, which is what Android's adbd
// speaks during wireless-debugging pairing. It is deliberately NOT RFC 9382 and
// not what any Go SPAKE2 package implements (`gospake2`, for instance, is
// python-spake2-compatible and would not interoperate), so it is written here
// against BoringSSL's crypto/curve25519/spake25519.c.
//
// Everything below that looks arbitrary is load-bearing: the M/N points, the
// SHA-512 of the password used two different ways, the role-dependent ordering
// of the transcript, and the little-endian 8-byte length prefix on every
// transcript field. A mismatch in any of them yields a key that differs from
// the device's, which surfaces as the pairing being rejected with no diagnosis.

type spake2Role int

const (
	spake2Alice spake2Role = iota // the adb client
	spake2Bob                     // adbd
)

// The two generator points SPAKE2 masks with, one per role. BoringSSL derives
// them from "edwards25519 point generation seed (M)" / "(N)" and hardcodes the
// result; the encodings are copied from the comment at the top of spake25519.c.
var (
	spakeM = mustDecodePoint("5ada7e4bf6ddd9adb6626d32131c6b5c51a1e347a3478f53cfcf441b88eed12e")
	spakeN = mustDecodePoint("10e3df0ae37d8e7a99b5fe74b44672103dbddcbd06af680d71329a11693bc778")
)

func mustDecodePoint(h string) *edwards25519.Point {
	b, err := hex.DecodeString(h)
	if err != nil {
		panic("adb: bad SPAKE2 point constant: " + err.Error())
	}
	p, err := new(edwards25519.Point).SetBytes(b)
	if err != nil {
		panic("adb: SPAKE2 point constant is not on the curve: " + err.Error())
	}
	return p
}

// scalarEight is used to give the ephemeral scalar a factor of eight, which is
// what lets the protocol ignore any small-order component in the peer's point.
var scalarEight = func() *edwards25519.Scalar {
	var b [32]byte
	b[0] = 8
	s, err := edwards25519.NewScalar().SetCanonicalBytes(b[:])
	if err != nil {
		panic(err)
	}
	return s
}()

type spake2 struct {
	role      spake2Role
	myName    []byte
	theirName []byte

	// private is r mod L — WITHOUT the factor of eight. BoringSSL stores
	// 8·r and multiplies with it directly; here the eight is applied to the
	// base point (for the outgoing message) and to the peer's point (via
	// MultByCofactor) instead. Both give the same result on a prime-order
	// point while keeping every scalar canonical, which this library requires.
	private        *edwards25519.Scalar
	passwordScalar *edwards25519.Scalar
	passwordHash   []byte // the full 64-byte SHA-512, which the transcript uses
	myMsg          []byte
}

func newSPAKE2(role spake2Role, myName, theirName string) *spake2 {
	return &spake2{role: role, myName: []byte(myName), theirName: []byte(theirName)}
}

// generateMsg produces this side's 32-byte SPAKE2 message.
func (s *spake2) generateMsg(password []byte) ([]byte, error) {
	var seed [64]byte
	if _, err := rand.Read(seed[:]); err != nil {
		return nil, err
	}
	priv, err := edwards25519.NewScalar().SetUniformBytes(seed[:])
	if err != nil {
		return nil, err
	}
	s.private = priv

	// P = 8·r·G. The eight matches BoringSSL's left_shift_3 of the private
	// scalar; folding it in here is equivalent because G has prime order.
	eightR := edwards25519.NewScalar().Multiply(priv, scalarEight)
	P := new(edwards25519.Point).ScalarBaseMult(eightR)

	// The password is hashed once and used twice: reduced to a scalar for the
	// mask, and kept whole for the transcript.
	sum := sha512.Sum512(password)
	s.passwordHash = sum[:]
	pwScalar, err := edwards25519.NewScalar().SetUniformBytes(sum[:])
	if err != nil {
		return nil, err
	}
	s.passwordScalar = pwScalar

	// mask = w·M for Alice, w·N for Bob.
	mask := new(edwards25519.Point).ScalarMult(pwScalar, s.maskPoint(s.role))
	s.myMsg = new(edwards25519.Point).Add(P, mask).Bytes()
	return append([]byte(nil), s.myMsg...), nil
}

// processMsg consumes the peer's message and returns the 64-byte shared key.
func (s *spake2) processMsg(theirMsg []byte) ([]byte, error) {
	if s.myMsg == nil {
		return nil, errors.New("adb: SPAKE2 message must be generated before processing the peer's")
	}
	if len(theirMsg) != 32 {
		return nil, fmt.Errorf("adb: SPAKE2 peer message is %d bytes, want 32", len(theirMsg))
	}
	QStar, err := new(edwards25519.Point).SetBytes(theirMsg)
	if err != nil {
		return nil, fmt.Errorf("adb: SPAKE2 peer point is not on the curve: %w", err)
	}

	// Unmask with the OTHER role's point, then strip the mask.
	peersMask := new(edwards25519.Point).ScalarMult(s.passwordScalar, s.maskPoint(other(s.role)))
	QExt := new(edwards25519.Point).Subtract(QStar, peersMask)

	// dh = 8·r·Q. Multiplying Q by the cofactor first kills any small-order
	// component the peer could have smuggled in, and is what BoringSSL's
	// factor-of-eight private scalar achieves.
	Q8 := new(edwards25519.Point).MultByCofactor(QExt)
	dh := new(edwards25519.Point).ScalarMult(s.private, Q8)

	// The transcript is always in Alice-then-Bob order regardless of which side
	// is computing it, so both ends hash the same bytes.
	h := sha512.New()
	aliceName, bobName := s.myName, s.theirName
	aliceMsg, bobMsg := s.myMsg, theirMsg
	if s.role == spake2Bob {
		aliceName, bobName = s.theirName, s.myName
		aliceMsg, bobMsg = theirMsg, s.myMsg
	}
	for _, field := range [][]byte{
		aliceName, bobName, aliceMsg, bobMsg, dh.Bytes(), s.passwordHash,
	} {
		writeLengthPrefixed(h, field)
	}
	return h.Sum(nil), nil
}

func (s *spake2) maskPoint(r spake2Role) *edwards25519.Point {
	if r == spake2Alice {
		return spakeM
	}
	return spakeN
}

func other(r spake2Role) spake2Role {
	if r == spake2Alice {
		return spake2Bob
	}
	return spake2Alice
}

// writeLengthPrefixed mirrors BoringSSL's update_with_length_prefix: an
// eight-byte LITTLE-endian length, then the bytes. (Little-endian here and
// big-endian in the pairing packet header — they are different layers and do
// not agree, which is exactly the kind of thing that is easy to get wrong.)
func writeLengthPrefixed(h interface{ Write([]byte) (int, error) }, data []byte) {
	var l [8]byte
	binary.LittleEndian.PutUint64(l[:], uint64(len(data)))
	h.Write(l[:])
	h.Write(data)
}
