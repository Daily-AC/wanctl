package policy

import (
	"testing"
)

// The elevated class exists so that two things stay separate: permission to run
// a command, and permission to run it as root. Every test here is a way that
// separation could quietly fail.

func TestBypassDoesNotCoverElevatedCommands(t *testing.T) {
	e := &Engine{mode: ModeBypass}
	if !e.Bypasses(KindExec) {
		t.Fatal("bypass stopped covering ordinary commands")
	}
	if e.Bypasses(KindExecElevated) {
		t.Fatal("bypass covered an elevated command: a device left in bypass so it can " +
			"work unattended would be handing out root to anything that can reach it")
	}
	for _, k := range []Kind{KindRead, KindWrite, KindLogs} {
		if !e.Bypasses(k) {
			t.Fatalf("bypass stopped covering %s", k)
		}
	}
}

func TestNormalModeBypassesNothing(t *testing.T) {
	e := &Engine{mode: ModeNormal}
	for _, k := range []Kind{KindExec, KindExecElevated, KindRead, KindWrite, KindLogs} {
		if e.Bypasses(k) {
			t.Fatalf("normal mode bypassed %s", k)
		}
	}
}

func TestExecRuleDoesNotAuthorizeTheElevatedForm(t *testing.T) {
	e := &Engine{rules: []Rule{{Kind: KindExec, Pattern: "pm list packages", Scope: ScopeGlobal}}}
	if !e.Allowed(Request{Kind: KindExec, Cmd: "pm list packages"}) {
		t.Fatal("the plain rule stopped authorizing the plain command")
	}
	if e.Allowed(Request{Kind: KindExecElevated, Cmd: "pm list packages"}) {
		t.Fatal("an exec rule authorized the same command as root; " +
			"approving a command is not approving a privilege")
	}
}

func TestElevatedRuleDoesNotAuthorizeThePlainForm(t *testing.T) {
	// The other direction matters less for safety but keeps the two grants from
	// blurring: a rule means one thing.
	e := &Engine{rules: []Rule{{Kind: KindExecElevated, Pattern: "id", Scope: ScopeGlobal}}}
	if !e.Allowed(Request{Kind: KindExecElevated, Cmd: "id"}) {
		t.Fatal("the elevated rule did not authorize the elevated command")
	}
	if e.Allowed(Request{Kind: KindExec, Cmd: "id"}) {
		t.Fatal("an elevated rule authorized the plain command")
	}
}

func TestElevatedRulesMatchLikeExecRules(t *testing.T) {
	// Prefix matching, the " *" suffix and the single-simple-command guard are
	// the same as for exec — the class is separate, not differently shaped.
	e := &Engine{rules: []Rule{{Kind: KindExecElevated, Pattern: "dumpsys *", Scope: ScopeGlobal}}}
	if !e.Allowed(Request{Kind: KindExecElevated, Cmd: "dumpsys battery"}) {
		t.Fatal("prefix rule did not match")
	}
	if e.Allowed(Request{Kind: KindExecElevated, Cmd: "dumpsys battery; rm -rf /"}) {
		t.Fatal("a prefix rule authorized a compound command")
	}
	if e.Allowed(Request{Kind: KindExecElevated, Cmd: "pm uninstall com.example"}) {
		t.Fatal("a dumpsys rule authorized pm")
	}
}

func TestElevatedRuleForCarriesTheElevatedKind(t *testing.T) {
	// A remembered decision must be stored in its own class; storing it as
	// KindExec would silently widen it into the unprivileged grant too.
	r := RuleFor(Request{Kind: KindExecElevated, Cmd: "input tap 100 200", Cwd: "/data"}, ScopeGlobal)
	if r.Kind != KindExecElevated {
		t.Fatalf("remembered rule kind = %s, want %s", r.Kind, KindExecElevated)
	}
	if r.Pattern != "input tap 100 200" {
		t.Fatalf("pattern = %q", r.Pattern)
	}
}

func TestElevatedDirScopedRuleHonoursCwd(t *testing.T) {
	e := &Engine{rules: []Rule{{Kind: KindExecElevated, Pattern: "id", Dir: "/data/local/tmp", Scope: ScopeDir}}}
	if !e.Allowed(Request{Kind: KindExecElevated, Cmd: "id", Cwd: "/data/local/tmp"}) {
		t.Fatal("dir-scoped elevated rule did not match its own directory")
	}
	if e.Allowed(Request{Kind: KindExecElevated, Cmd: "id", Cwd: "/sdcard"}) {
		t.Fatal("dir-scoped elevated rule matched another directory")
	}
}
