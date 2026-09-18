package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
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

// spoofCmd is a shell command wearing a token's clothes. Canonical says it is
// not a script; every display path must therefore show all of it, because the
// half after the semicolon is the half that runs.
const spoofCmd = "script:sh:0123456789abcdef; printf REVIEW_EXECUTED"

// An approval prompt that hides what it is asking about is worse than no
// prompt: the human says yes to something they were never shown. CommandLabel
// used to hand any script-shaped text to Short, which truncated it (review of
// #108, 2026-09-18).
func TestCommandLabelNeverHidesPartOfACommand(t *testing.T) {
	cases := []string{
		spoofCmd,
		"script:sh:" + strings.Repeat("0", 64) + " && rm -rf /",
		"script:powershell:" + strings.Repeat("a", 64) + "; Remove-Item x",
		// Shaped like a token but not one: unknown interpreter, wrong digest
		// length, upper case, non-hex.
		"script:python:" + strings.Repeat("0", 64),
		"script:sh:" + strings.Repeat("0", 63),
		"script:sh:" + strings.Repeat("A", 64),
		"script:sh:" + strings.Repeat("z", 64),
		"script:sh:0123456789abcdef",
		"script:sh:",
		"script:",
	}
	for _, cmd := range cases {
		if _, ok := script.Canonical(cmd); ok {
			t.Fatalf("%q was taken for a script; this test assumes it is not one", cmd)
		}
		if got := CommandLabel(cmd); got != cmd {
			t.Errorf("CommandLabel(%q) = %q — a decision was shown less than it is deciding", cmd, got)
		}
		if got := CommandPattern(cmd); got != cmd {
			t.Errorf("CommandPattern(%q) = %q, want the command itself", cmd, got)
		}
	}
}

// The terminal prompt is one of the two places that label is read. It must
// print the whole command.
func TestConsoleApproverPromptShowsTheWholeCommand(t *testing.T) {
	for _, kind := range []Kind{KindExec, KindExecElevated} {
		var out strings.Builder
		d := NewConsoleApprover(strings.NewReader("y\n"), &out).Ask(Request{Kind: kind, Cmd: spoofCmd, Peer: "someone"})
		if !d.Allow {
			t.Fatalf("%s: the prompt did not read the answer", kind)
		}
		if !strings.Contains(out.String(), spoofCmd) {
			t.Fatalf("%s prompt showed:\n%s\nwant the whole command %q", kind, out.String(), spoofCmd)
		}
		if strings.Contains(out.String(), "…") {
			t.Fatalf("%s prompt abbreviated a command that is not a script:\n%s", kind, out.String())
		}
	}
}

// Boundary payloads for both transports. Nothing here comes from Command --
// the 1 MiB case is far past its size cap -- so it is all input a controller
// could choose.
func TestCanonicalBoundaryPayloads(t *testing.T) {
	wrap := func(interp script.Interp, payload string) string {
		if interp == script.POSIX {
			return "printf %s '" + payload + "' | base64 -d | sh"
		}
		return "powershell -NoProfile -NonInteractive -EncodedCommand '" + payload + "'"
	}
	for _, interp := range []script.Interp{script.POSIX, script.PowerShell} {
		for _, n := range []int{0, 1, 18000, 1 << 20} {
			raw := bytes.Repeat([]byte("a"), n)
			cmd := wrap(interp, base64.StdEncoding.EncodeToString(raw))
			sum := sha256.Sum256(raw)
			want := script.CanonicalPrefix + string(interp) + ":" + hex.EncodeToString(sum[:])
			tok, ok := script.Canonical(cmd)
			if !ok || tok != want {
				t.Fatalf("%s/%d bytes: Canonical = (%q, %v), want %q", interp, n, tok, ok, want)
			}
			if CommandPattern(cmd) != want {
				t.Fatalf("%s/%d bytes: the rule would not carry the token", interp, n)
			}
			if !MatchCommand(want, cmd) {
				t.Fatalf("%s/%d bytes: the token did not match its own script", interp, n)
			}
			if MatchCommand(want, want) || MatchCommand(want, want+" --now") {
				t.Fatalf("%s/%d bytes: the token matched as a command line", interp, n)
			}
			if MatchCommand(want[:len(want)-48], cmd) {
				t.Fatalf("%s/%d bytes: a truncated token matched", interp, n)
			}
		}
		for _, payload := range []string{"not base64!", "' ; printf injected; #", strings.Repeat("!", 1<<20)} {
			cmd := wrap(interp, payload)
			if tok, ok := script.Canonical(cmd); ok {
				t.Fatalf("%s: an undecodable payload became %q", interp, tok)
			}
			if CommandPattern(cmd) != cmd || CommandLabel(cmd) != cmd {
				t.Fatalf("%s: an undecodable payload was rewritten for display or storage", interp)
			}
			if MatchCommand(script.CanonicalPrefix+string(interp)+":"+strings.Repeat("0", 64), cmd) {
				t.Fatalf("%s: an undecodable payload matched a script rule", interp)
			}
		}
	}
}

// A rules file written before this change stores the whole base64 transport;
// one written after stores the digest. Both have to keep working, side by side
// and across a reload, for both interpreters.
func TestLegacyPayloadRulesAndDigestRulesCoexist(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WANCTL_CONFIG_DIR", dir)

	var rules []Rule
	var commands []string
	for _, interp := range []script.Interp{script.POSIX, script.PowerShell} {
		for _, legacy := range []bool{true, false} {
			body := "echo new\n"
			if legacy {
				body = "echo old\n"
			}
			cmd, err := script.Command(interp, []byte(body))
			if err != nil {
				t.Fatal(err)
			}
			commands = append(commands, cmd)
			r := RuleFor(Request{Kind: KindExecElevated, Cmd: cmd}, ScopeGlobal)
			if legacy {
				r.Pattern = cmd // what an older agent wrote
			}
			rules = append(rules, r)
		}
	}
	data, err := json.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rules.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	e, err := Open("rules.json", ModeNormal)
	if err != nil {
		t.Fatal(err)
	}
	listed := e.List()
	if len(listed) != len(rules) {
		t.Fatalf("loaded %d rules, want %d", len(listed), len(rules))
	}
	for i, cmd := range commands {
		if listed[i].Pattern != rules[i].Pattern {
			t.Fatalf("rule %d came back as %q, want %q", i, listed[i].Pattern, rules[i].Pattern)
		}
		if !e.Allowed(Request{Kind: KindExecElevated, Cmd: cmd}) {
			t.Fatalf("rule %d (%s) stopped matching its script", i, patternKind(rules[i].Pattern))
		}
		if e.Allowed(Request{Kind: KindExec, Cmd: cmd}) {
			t.Fatalf("rule %d authorized the unelevated form", i)
		}
	}
	other, err := script.Command(script.POSIX, []byte("echo other\n"))
	if err != nil {
		t.Fatal(err)
	}
	if e.Allowed(Request{Kind: KindExecElevated, Cmd: other}) {
		t.Fatal("a script none of the rules names was authorized")
	}
}

func patternKind(p string) string {
	if strings.HasPrefix(p, script.CanonicalPrefix) {
		return "digest"
	}
	return "legacy payload"
}
