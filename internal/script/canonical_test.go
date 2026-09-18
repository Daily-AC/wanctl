package script

import (
	"crypto/sha256"
	"strings"
	"testing"
)

func TestCanonicalIsStableAndContentAddressed(t *testing.T) {
	for _, interp := range []Interp{POSIX, PowerShell} {
		a, err := Command(interp, []byte("echo one\n"))
		if err != nil {
			t.Fatal(err)
		}
		b, err := Command(interp, []byte("echo one\n"))
		if err != nil {
			t.Fatal(err)
		}
		other, err := Command(interp, []byte("echo two\n"))
		if err != nil {
			t.Fatal(err)
		}
		ta, ok := Canonical(a)
		if !ok {
			t.Fatalf("%s: a generated command was not recognised: %s", interp, a)
		}
		tb, _ := Canonical(b)
		to, _ := Canonical(other)
		if ta != tb {
			t.Fatalf("%s: the same script produced two tokens: %q and %q", interp, ta, tb)
		}
		if ta == to {
			t.Fatalf("%s: two different scripts share a token (%q) — a rule "+
				"remembered for one would authorize the other", interp, ta)
		}
		if !strings.HasPrefix(ta, CanonicalPrefix+string(interp)+":") {
			t.Fatalf("%s: token %q does not name its interpreter", interp, ta)
		}
	}
}

// Anything that is not one of the two generated transports is an ordinary
// command line and must be left alone — the policy layer keys off ok.
func TestCanonicalRejectsOrdinaryCommands(t *testing.T) {
	for _, c := range []string{
		"",
		"pm install -r /sdcard/app.apk",
		"printf %s 'not base64!' | base64 -d | sh",
		"printf %s 'aGk=' | base64 -d | sh; rm -rf /",
		"echo printf %s 'aGk=' | base64 -d | sh",
		"powershell -NoProfile -EncodedCommand 'aGk='",
	} {
		if tok, ok := Canonical(c); ok {
			t.Errorf("%q was taken for a script transport (%q)", c, tok)
		}
	}
}

// The prefix and the suffix of the POSIX transport share their quote, so a
// command can satisfy HasPrefix and HasSuffix while being shorter than both
// together. Slicing it panicked — from inside the text of a REFUSAL, which any
// paired controller could reach without permission to run anything (review of
// #108). Every one of these must come back "not a script", not die.
func TestCanonicalSurvivesOverlappingAndTruncatedInput(t *testing.T) {
	for _, c := range []string{
		"printf %s ' | base64 -d | sh",
		"printf %s '| base64 -d | sh",
		" printf %s ' | base64 -d | sh ",
		"powershell -NoProfile -NonInteractive -EncodedCommand '",
		"'",
		"printf %s '",
		// Not listed: `… -EncodedCommand ''`, which is a well-formed transport
		// carrying an empty script. It is recognised, gets the empty string's
		// digest, and is then gated like anything else — there is nothing to
		// reject.
	} {
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("Canonical(%q) panicked: %v", c, p)
				}
			}()
			if tok, ok := Canonical(c); ok {
				t.Errorf("Canonical(%q) = %q, want it to be taken for an ordinary command", c, tok)
			}
		}()
	}
}

// The token is the authorization, so it carries the whole digest: a truncated
// one turns "find a second script that passes" into a birthday search over the
// truncation.
func TestCanonicalCarriesTheWholeDigest(t *testing.T) {
	cmd, err := Command(POSIX, []byte("id\n"))
	if err != nil {
		t.Fatal(err)
	}
	tok, ok := Canonical(cmd)
	if !ok {
		t.Fatal("a generated command was not recognised")
	}
	digest := strings.TrimPrefix(tok, CanonicalPrefix+string(POSIX)+":")
	if len(digest) != sha256.Size*2 {
		t.Fatalf("digest %q is %d hex characters, want %d — the whole SHA-256",
			digest, len(digest), sha256.Size*2)
	}
}

// Short is for eyes: a visible prefix of the real token, marked as abbreviated
// so nobody pastes it into --pattern and wonders why it never matches.
func TestShortAbbreviatesOnlyTokens(t *testing.T) {
	cmd, _ := Command(POSIX, []byte("id\n"))
	tok, _ := Canonical(cmd)
	short := Short(tok)
	if short == tok {
		t.Fatalf("Short did not abbreviate %q", tok)
	}
	if !strings.HasSuffix(short, "…") {
		t.Fatalf("Short(%q) = %q, want it to say it is abbreviated", tok, short)
	}
	if !strings.HasPrefix(tok, strings.TrimSuffix(short, "…")) {
		t.Fatalf("Short(%q) = %q is not a prefix of the token a person would compare it to", tok, short)
	}
	for _, other := range []string{"pm install -r /sdcard/app.apk", "", "script:", "script:sh", "script:sh:abcd"} {
		if got := Short(other); got != other {
			t.Errorf("Short(%q) = %q, want it unchanged", other, got)
		}
	}
}
