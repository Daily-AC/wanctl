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
	if strings.Contains(r.Pattern, "base64") || len(r.Pattern) > 96 {
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

// A command whose text IS the token is not the script the token names. Before
// the script branch moved ahead of the string-equality allow, a remembered
// script rule matched that literal string — and a file of that name on PATH
// then ran under the script's grant (review of #108, 2026-09-18).
func TestLiteralTokenIsNotTheScriptItNames(t *testing.T) {
	cmd := scriptCmd(t, "printf approved-script\n")
	r := RuleFor(Request{Kind: KindExecElevated, Cmd: cmd}, ScopeGlobal)
	e := &Engine{rules: []Rule{r}}
	if e.Allowed(Request{Kind: KindExecElevated, Cmd: r.Pattern}) {
		t.Fatalf("a command equal to the pattern %q was authorized by it; "+
			"an executable of that name would run under the script's grant", r.Pattern)
	}
	if e.Allowed(Request{Kind: KindExecElevated, Cmd: r.Pattern + " --now"}) {
		t.Fatal("the token behaved as a command prefix")
	}
	if !e.Allowed(Request{Kind: KindExecElevated, Cmd: cmd}) {
		t.Fatal("the rule stopped matching the script it names")
	}
}

// The persisted pattern is the whole digest; only what a human is shown is cut
// short, and never compared.
func TestScriptRuleStoresTheWholeDigest(t *testing.T) {
	cmd := scriptCmd(t, "id\n")
	r := RuleFor(Request{Kind: KindExecElevated, Cmd: cmd}, ScopeGlobal)
	full, _ := script.Canonical(cmd)
	if r.Pattern != full {
		t.Fatalf("stored pattern = %q, want the full token %q", r.Pattern, full)
	}
	label := CommandLabel(cmd)
	if label == full {
		t.Fatal("the approval label was not abbreviated")
	}
	if !strings.HasPrefix(full, strings.TrimSuffix(label, "…")) {
		t.Fatalf("label %q is not a visible prefix of the rule %q", label, full)
	}
	// And the abbreviation is a label, not a key.
	e := &Engine{rules: []Rule{{Kind: KindExecElevated, Pattern: label, Scope: ScopeGlobal}}}
	if e.Allowed(Request{Kind: KindExecElevated, Cmd: cmd}) {
		t.Fatal("a truncated token matched a script; the digest must be compared whole")
	}
}

// Nothing on the policy path may panic on a command a stranger chose. The
// refusal text, the approval card and rule matching all go through these.
func TestMalformedScriptShapedCommandsDoNotPanic(t *testing.T) {
	e := &Engine{rules: []Rule{
		{Kind: KindExecElevated, Pattern: "script:sh:" + strings.Repeat("ab", 32), Scope: ScopeGlobal},
		{Kind: KindExec, Pattern: "*", Scope: ScopeGlobal},
	}}
	for _, c := range []string{
		"printf %s ' | base64 -d | sh",
		"powershell -NoProfile -NonInteractive -EncodedCommand '",
		"printf %s '",
		"'",
	} {
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("%q panicked on the policy path: %v", c, p)
				}
			}()
			if e.Allowed(Request{Kind: KindExecElevated, Cmd: c}) {
				t.Errorf("%q matched a script rule", c)
			}
			CommandPattern(c)
			CommandLabel(c)
		}()
	}
}
