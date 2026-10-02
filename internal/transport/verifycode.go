package transport

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"unicode"
)

// First-contact verification codes.
//
// The pin in Store is only as good as the comparison a human makes before
// recording it, and until now that comparison was the whole "SHA256:<43 base64
// characters>" fingerprint: two screens, one wrong character indistinguishable
// from an impersonation, and nothing an AI-driven controller — which has no
// camera and no phone — can do at all.
//
// A verification code replaces that comparison with nine digits without
// weakening it. The device derives the code locally from its own certificate
// and a number the controller generated for this one dial; the controller
// derives the same code from the certificate that dial presented. The number is
// what makes shortening safe: a truncated fingerprint alone is forgeable,
// because the relay already knows the real device's fingerprint and can grind a
// key pair whose truncated digest matches it, while a certificate presented for
// a number that did not exist yet cannot be ground at all.
//
// The number reaches the device through the human, never over the wire, and
// neither side transmits its code: the comparison is the human's, and it stays
// local, so no relay, portal or resolver can forge a match.
const (
	// verifyDomain separates this derivation from every other hash of a
	// fingerprint in the tree.
	verifyDomain = "wanctl/verify/v1"

	// VerifyNumberDigits is the length of the per-dial number the controller
	// generates and the device owner types in. Six digits prints as one
	// grouped value a person reads off one screen and types into the other; it
	// needs no secrecy, because the certificate is already fixed by the time
	// it exists.
	VerifyNumberDigits = 6

	// VerifyCodeDigits is the length of the code both sides compare. Nine
	// digits leave a substituted certificate one chance in a billion per dial
	// of colliding, and a mismatch is visible to the human every other time.
	VerifyCodeDigits = 9

	// verifyNumberSpace and verifyCodeSpace are the inclusive-exclusive ranges
	// the digits above name.
	verifyNumberSpace = 1_000_000     // 10^6
	verifyCodeSpace   = 1_000_000_000 // 10^9
)

// cutoff is the largest uint32 multiple of space. Values at or above it are
// rejected rather than reduced, so every code in the space is exactly as likely
// as every other — "close enough" is not a property worth reasoning about in a
// value an attacker gets to grind.
func cutoff(space uint32) uint32 {
	max := uint32(math.MaxUint32)
	return max - (max % space)
}

// NewVerifyNumber returns a fresh per-dial verification number: uniformly
// random decimal digits from the system CSPRNG.
func NewVerifyNumber() string {
	return fmt.Sprintf("%0*d", VerifyNumberDigits, uniformBelow(verifyNumberSpace))
}

// uniformBelow returns a uniformly random value in [0, n), for n > 0.
func uniformBelow(n uint32) uint32 {
	limit := cutoff(n)
	var buf [4]byte
	for {
		rand.Read(buf[:]) // Go 1.26 terminates the process if the CSPRNG fails.
		if v := binary.BigEndian.Uint32(buf[:]); v < limit {
			return v % n
		}
	}
}

// VerifyCode derives the nine-digit code a human compares. fingerprint is the
// device's certificate fingerprint, in the canonical "SHA256:<base64>" form, so
// the device can compute it from its own identity and the controller from the
// certificate that dial presented; number is the per-dial number from
// NewVerifyNumber, accepted with or without its grouping spaces.
func VerifyCode(fingerprint, number string) (string, error) {
	fp := strings.TrimSpace(fingerprint)
	if !ValidFingerprint(fp) {
		return "", fmt.Errorf("verification code: %q is not a SHA256: fingerprint", fingerprint)
	}
	num, err := NormalizeVerifyNumber(number)
	if err != nil {
		return "", err
	}
	// Each candidate window is rejected with probability ~7%, and the counter
	// keeps the derivation a pure function of (fingerprint, number) when that
	// happens.
	for i := 0; ; i++ {
		h := sha256.New()
		h.Write([]byte(verifyDomain))
		h.Write([]byte{0})
		h.Write([]byte(fp))
		h.Write([]byte{0})
		h.Write([]byte(num))
		if i > 0 {
			h.Write([]byte{byte(i)})
		}
		sum := h.Sum(nil)
		for off := 0; off+4 <= len(sum); off += 4 {
			if v := binary.BigEndian.Uint32(sum[off : off+4]); v < cutoff(verifyCodeSpace) {
				return fmt.Sprintf("%0*d", VerifyCodeDigits, v%verifyCodeSpace), nil
			}
		}
	}
}

// NormalizeVerifyNumber strips the grouping a human reads (spaces, dashes) and
// validates the result, so "482 913", "482-913" and "482913" are one number.
func NormalizeVerifyNumber(number string) (string, error) {
	var b strings.Builder
	for _, r := range number {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case unicode.IsSpace(r) || r == '-':
			// Grouping the human was told to ignore, and whatever a paste
			// brings with it.
		default:
			return "", fmt.Errorf("verification code: %q is not a %d-digit number", number, VerifyNumberDigits)
		}
	}
	digits := b.String()
	if len(digits) != VerifyNumberDigits {
		return "", fmt.Errorf("verification code: %q is not a %d-digit number", number, VerifyNumberDigits)
	}
	return digits, nil
}

// NormalizeVerifyCode is NormalizeVerifyNumber for the compared code.
func NormalizeVerifyCode(code string) (string, error) {
	digits := onlyDigits(code)
	if len(digits) != VerifyCodeDigits {
		return "", fmt.Errorf("verification code: %q is not a %d-digit code", code, VerifyCodeDigits)
	}
	return digits, nil
}

// GroupDigits groups a digit string in threes from the left — "482913" prints
// as "482 913" and "771204638" as "771 204 638" — which is how both values are
// shown to the human who has to read them off a screen.
func GroupDigits(s string) string {
	digits := onlyDigits(s)
	var b strings.Builder
	for i, r := range digits {
		if i > 0 && (len(digits)-i)%3 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func onlyDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
