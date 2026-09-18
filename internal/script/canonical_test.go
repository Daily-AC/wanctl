package script

import (
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
