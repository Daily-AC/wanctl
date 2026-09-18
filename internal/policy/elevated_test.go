package policy

import (
	"testing"
)

// The elevated class exists so that two things stay separate: permission to run
// a command, and permission to run it as root. Every test here is a way that
// separation could quietly fail.

// Bypass alone is not consent to elevation: a device left in bypass so it can
// work unattended, with no elevation channel switched on, refuses exactly as it
// always did.
func TestBypassWithoutTheElevationChannelDoesNotCoverElevatedCommands(t *testing.T) {
	e := &Engine{mode: ModeBypass}
	const elevationOff = false
	if !e.Bypasses(KindExec, elevationOff) {
		t.Fatal("bypass stopped covering ordinary commands")
	}
	if e.Bypasses(KindExecElevated, elevationOff) {
		t.Fatal("bypass with the elevation channel off covered an elevated command: " +
			"one switch is not the two decisions this needs")
	}
	for _, k := range []Kind{KindRead, KindWrite, KindLogs} {
		if !e.Bypasses(k, elevationOff) {
			t.Fatalf("bypass stopped covering %s", k)
		}
	}
}

// The owner's decision of 2026-09-18 (issue #71, option A): 自动放行所有命令 and
// 提权通道 are two explicit, separately-defaulted-off opt-ins, and having made
// both is the consent. Requiring a third per-command approval made elevation
// unusable on exactly the unattended devices bypass mode exists for.
func TestBypassWithTheElevationChannelCoversElevatedCommands(t *testing.T) {
	e := &Engine{mode: ModeBypass}
	if !e.Bypasses(KindExecElevated, true) {
		t.Fatal("bypass + elevation channel on still refused an elevated command")
	}
}

func TestNormalModeBypassesNothing(t *testing.T) {
	e := &Engine{mode: ModeNormal}
	for _, k := range []Kind{KindExec, KindExecElevated, KindRead, KindWrite, KindLogs} {
		for _, elevation := range []bool{false, true} {
			if e.Bypasses(k, elevation) {
				t.Fatalf("normal mode bypassed %s (elevation=%v)", k, elevation)
			}
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
