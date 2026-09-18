package policy

import (
	"strings"
	"testing"

	"wanctl/internal/script"
)

// `wanctl exec --script` never sends the script: it sends
// `printf %s '<base64>' | base64 -d | sh`. Remembering that verbatim produced a
// rule that could not be read on an approval card, listed usefully, or retyped
// into `wanctl rules add` — so "allow + remember" was unusable for scripts
// (#63 "Related"). A remembered script rule names the script instead.

func scriptCmd(t *testing.T, body string) string {
	t.Helper()
	cmd, err := script.Command(script.POSIX, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return cmd
}

func TestRememberedScriptRuleNamesTheScriptNotTheBlob(t *testing.T) {
	cmd := scriptCmd(t, "pm install -r /sdcard/Download/app.apk\n")
	r := RuleFor(Request{Kind: KindExecElevated, Cmd: cmd}, ScopeGlobal)
	if !strings.HasPrefix(r.Pattern, script.CanonicalPrefix) {
		t.Fatalf("pattern = %q, want a %s… token", r.Pattern, script.CanonicalPrefix)
	}
	if strings.Contains(r.Pattern, "base64") || len(r.Pattern) > 64 {
		t.Fatalf("pattern is still the transport blob: %q", r.Pattern)
	}
	e := &Engine{rules: []Rule{r}}
	if !e.Allowed(Request{Kind: KindExecElevated, Cmd: cmd}) {
		t.Fatal("the remembered rule did not match a re-run of the same script")
	}
}

func TestScriptRuleIsAsNarrowAsTheScript(t *testing.T) {
	r := RuleFor(Request{Kind: KindExecElevated, Cmd: scriptCmd(t, "id\n")}, ScopeGlobal)
	e := &Engine{rules: []Rule{r}}
	if e.Allowed(Request{Kind: KindExecElevated, Cmd: scriptCmd(t, "rm -rf /sdcard\n")}) {
		t.Fatal("a rule remembered for one script authorized a different one: " +
			"the token must not be a grant over the --script transport")
	}
	// And it is still a class of its own: the same script unelevated is a
	// different grant.
	if e.Allowed(Request{Kind: KindExec, Cmd: scriptCmd(t, "id\n")}) {
		t.Fatal("an elevated script rule authorized the unelevated form")
	}
}

func TestScriptTokenIsNotMatchedByCommandText(t *testing.T) {
	// The pattern names a script; a command that happens to spell the token out
	// is not that script.
	tok, ok := script.Canonical(scriptCmd(t, "id\n"))
	if !ok {
		t.Fatal("the generated script command was not recognised as one")
	}
	e := &Engine{rules: []Rule{{Kind: KindExec, Pattern: tok, Scope: ScopeGlobal}}}
	if e.Allowed(Request{Kind: KindExec, Cmd: "echo " + tok}) {
		t.Fatalf("a command mentioning %q matched the script rule", tok)
	}
}

func TestOrdinaryCommandPatternsAreUntouched(t *testing.T) {
	r := RuleFor(Request{Kind: KindExecElevated, Cmd: "pm install -r /sdcard/app.apk"}, ScopeGlobal)
	if r.Pattern != "pm install -r /sdcard/app.apk" {
		t.Fatalf("pattern = %q, want the command itself", r.Pattern)
	}
}

// A script token is typeable, which is the point: it is what makes
// `wanctl rules add --kind exec-elevated --pattern script:sh:…` a real way to
// pre-authorize a scripted elevated command.
func TestScriptTokenIsTypeable(t *testing.T) {
	tok, _ := script.Canonical(scriptCmd(t, "id\n"))
	for _, r := range tok {
		if r <= ' ' || r > '~' {
			t.Fatalf("token %q contains a character nobody can type: %q", tok, r)
		}
	}
}
