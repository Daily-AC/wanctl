package transport

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// The code a human compares has to be derivable twice from the same two inputs
// — once on the device, from its own certificate, and once on the controller,
// from the certificate the dial presented — or the comparison it replaces the
// fingerprint with cannot be made at all. Everything here exists to keep that
// pair of derivations identical, and to keep the grouping a person reads
// ("482 913") out of the value itself.
func TestVerifyCodeIsTheSameOnBothSidesAndMovesWithItsInputs(t *testing.T) {
	const fp = "SHA256:tqRqWcIwdooRPk+X53/ZUg+hb8bz1eDldaCEbEXC3kg="
	other := "SHA256:PkBvOpK0S0fRZ0eS2Ib3EGQMhVPfsoTrN0Fxo3gIEvE="

	code, err := VerifyCode(fp, "482913")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9]{9}$`).MatchString(code) {
		t.Fatalf("code %q is not nine digits", code)
	}
	// The grouping is presentation: the same number typed with or without it
	// must derive the same code, or the human's transcription decides the
	// answer.
	for _, spelling := range []string{"482913", "482 913", "482-913", " 482913 \n"} {
		got, err := VerifyCode(fp, spelling)
		if err != nil {
			t.Fatalf("number %q: %v", spelling, err)
		}
		if got != code {
			t.Errorf("number %q derived %s, want %s", spelling, got, code)
		}
	}
	if got, _ := VerifyCode(other, "482913"); got == code {
		t.Error("a different certificate derived the same code for one number")
	}
	if got, _ := VerifyCode(fp, "482914"); got == code {
		t.Error("a different number derived the same code for one certificate")
	}
	if got, _ := VerifyCode(fp, "482913"); got != code {
		t.Errorf("derivation is not deterministic: %s then %s", code, got)
	}
}

// A truncated fingerprint is grindable and a length check is what keeps the
// derivation off values that are not fingerprints at all, so both inputs are
// validated rather than hashed as given.
func TestVerifyCodeRefusesAnythingThatIsNotAFingerprintAndANumber(t *testing.T) {
	const fp = "SHA256:tqRqWcIwdooRPk+X53/ZUg+hb8bz1eDldaCEbEXC3kg="
	for _, tc := range []struct{ name, fp, number string }{
		{"empty fingerprint", "", "482913"},
		{"fingerprint without the prefix", strings.TrimPrefix(fp, "SHA256:"), "482913"},
		{"truncated fingerprint", "SHA256:tqRqWcIwdooRPk+X53/ZUg+hb8bz1eDldaCE", "482913"},
		{"number too short", fp, "48291"},
		{"number too long", fp, "4829134"},
		{"number with a letter", fp, "48291a"},
		{"number with a comma", fp, "482,913"},
		{"empty number", fp, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, err := VerifyCode(tc.fp, tc.number); err == nil {
				t.Fatalf("accepted %q/%q as %s", tc.fp, tc.number, code)
			}
		})
	}
}

// The derivation is versioned ("wanctl/verify/v1") because a controller and a
// device have to agree on it across versions, and a device is updated on its own
// schedule. Every other test here derives both sides with this same function, so
// none of them would notice the domain string, the separators, the window order
// or the digit count moving — while an older controller and a newer device would
// simply stop agreeing, which is the one failure this feature exists to prevent.
//
// This vector was reproduced through the built binary and by an independent
// implementation of the documented one-liner. It also exercises the rejection
// path: the digest's first window is above the cutoff, so the code comes from
// the second.
func TestVerifyCodeIsPinnedToTheV1Derivation(t *testing.T) {
	const fp = "SHA256:MBCKKNlgr5oIhC68F2aZmZzYCga3gXM6kORkaapRxFM="
	got, err := VerifyCode(fp, "482913")
	if err != nil {
		t.Fatal(err)
	}
	if got != "118026157" {
		t.Fatalf("VerifyCode(%s, 482913) = %s, want the v1 code 118026157", fp, got)
	}
}

// The number is fresh per dial and never leaves the two screens it is shown on.
// Freshness is the whole reason a nine-digit code can stand in for a
// fingerprint: a certificate is fixed before the number exists, so nothing can
// be ground against a value chosen after the fact.
func TestVerifyNumberIsFreshAndSixDigits(t *testing.T) {
	seen := map[string]bool{}
	firstDigits := map[byte]int{}
	for range 2000 {
		n := NewVerifyNumber()
		if !regexp.MustCompile(`^[0-9]{6}$`).MatchString(n) {
			t.Fatalf("number %q is not six digits", n)
		}
		seen[n] = true
		firstDigits[n[0]]++
	}
	// 2000 draws from 10^6 values: a repeat is possible, a collapse is not.
	if len(seen) < 1990 {
		t.Errorf("only %d distinct numbers in 2000 draws", len(seen))
	}
	if len(firstDigits) != 10 {
		t.Errorf("leading digits covered %d of 10 values: %v", len(firstDigits), firstDigits)
	}
}

// The code is what a human reads aloud, so its digits have to spread across the
// whole space rather than clustering where the derivation's first candidate
// window happens to land.
func TestVerifyCodeSpreadsAcrossItsDigits(t *testing.T) {
	const fp = "SHA256:tqRqWcIwdooRPk+X53/ZUg+hb8bz1eDldaCEbEXC3kg="
	leading := map[byte]int{}
	codes := map[string]bool{}
	for i := range 2000 {
		number := fmt.Sprintf("%06d", i)
		code, err := VerifyCode(fp, number)
		if err != nil {
			t.Fatal(err)
		}
		leading[code[0]]++
		codes[code] = true
	}
	if len(leading) != 10 {
		t.Errorf("leading digits covered %d of 10 values: %v", len(leading), leading)
	}
	if len(codes) < 1990 {
		t.Errorf("only %d distinct codes in 2000 derivations", len(codes))
	}
}

// Both values are read off a screen and typed back, so the grouping is part of
// the contract: it is what makes nine digits readable at a glance.
func TestGroupDigitsReadsInThrees(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"482913", "482 913"},
		{"771204638", "771 204 638"},
		{"482 913", "482 913"},
		{"482-913", "482 913"},
		{"", ""},
	} {
		if got := GroupDigits(tc.in); got != tc.want {
			t.Errorf("GroupDigits(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
