package policy

import (
	"path/filepath"
	"testing"
)

func TestMatchCommand(t *testing.T) {
	cases := []struct {
		p, c string
		want bool
	}{
		{"git status", "git status", true},
		{"git status", "git status -s", true},
		{"git status", "git statusx", false},
		{"git status", "git", false},
		{"npm run *", "npm run build", true},
		{"npm run *", "npm test", false},
		{"*", "anything goes", true},
	}
	for _, c := range cases {
		if got := MatchCommand(c.p, c.c); got != c.want {
			t.Errorf("MatchCommand(%q,%q)=%v want %v", c.p, c.c, got, c.want)
		}
	}
}

func TestMatchCommandRejectsAdditionalShellOperations(t *testing.T) {
	cases := []string{
		"git status && rm -rf /tmp/project",
		"git status; rm -rf /tmp/project",
		"git status ; rm -rf /tmp/project",
		"git status || rm -rf /tmp/project",
		"git status | sh",
		"git status\nrm -rf /tmp/project",
		"git status \nrm -rf /tmp/project",
		"git status & rm -rf /tmp/project",
		"git status $(rm -rf /tmp/project)",
		"git status <(rm -rf /tmp/project)",
		"git status > /tmp/status",
	}
	for _, command := range cases {
		if MatchCommand("git status", command) {
			t.Errorf("git status rule unexpectedly authorized %q", command)
		}
	}
}

func TestMatchCommandPreservesArgumentPrefixSemantics(t *testing.T) {
	cases := []string{
		"git status --short",
		"git status --porcelain=v1",
		`git status -- "path with spaces"`,
	}
	for _, command := range cases {
		if !MatchCommand("git status", command) {
			t.Errorf("git status rule should authorize argv command %q", command)
		}
	}
}

func TestMatchCommandAllowsExplicitExactCompoundRule(t *testing.T) {
	const command = "git status && git diff"
	if !MatchCommand(command, command) {
		t.Fatal("an exact rule should authorize the exact compound command")
	}
	if MatchCommand(command, command+" && rm -rf /tmp/project") {
		t.Fatal("an exact compound rule should not authorize an appended command")
	}
}

func TestWithin(t *testing.T) {
	if !Within("", "/anything") {
		t.Error("empty dir should match any path")
	}
	if !Within("/home/me", "/home/me/sub/file.txt") {
		t.Error("nested path should match")
	}
	if Within("/home/me", "/home/menace") {
		t.Error("prefix-but-not-subdir should not match")
	}
}

func TestExecScope(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	e, err := Open("rules.json", ModeNormal)
	if err != nil {
		t.Fatal(err)
	}
	// Global exec rule applies anywhere.
	e.Add(Rule{Kind: KindExec, Pattern: "ls", Scope: ScopeGlobal})
	if !e.Allowed(Request{Kind: KindExec, Cmd: "ls -la", Cwd: "/tmp"}) {
		t.Fatal("global exec rule should apply")
	}
	// Dir-scoped exec rule applies only in its dir.
	e.Add(Rule{Kind: KindExec, Pattern: "make", Dir: "/proj", Scope: ScopeDir})
	if !e.Allowed(Request{Kind: KindExec, Cmd: "make build", Cwd: "/proj"}) {
		t.Fatal("dir exec rule should apply in dir")
	}
	if e.Allowed(Request{Kind: KindExec, Cmd: "make build", Cwd: "/other"}) {
		t.Fatal("dir exec rule should NOT apply elsewhere")
	}
}

func TestFileReadWrite(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	e, _ := Open("rules.json", ModeNormal)
	// A write rule on a dir grants both read and write within it.
	e.Add(Rule{Kind: KindWrite, Pattern: "/data", Scope: ScopeDir})
	if !e.Allowed(Request{Kind: KindWrite, Path: "/data/a.txt"}) {
		t.Fatal("write rule should allow write")
	}
	if !e.Allowed(Request{Kind: KindRead, Path: "/data/a.txt"}) {
		t.Fatal("write rule should imply read")
	}
	// A read rule does NOT grant write.
	e.Add(Rule{Kind: KindRead, Pattern: "/logs", Scope: ScopeDir})
	if !e.Allowed(Request{Kind: KindRead, Path: "/logs/x"}) {
		t.Fatal("read rule should allow read")
	}
	if e.Allowed(Request{Kind: KindWrite, Path: "/logs/x"}) {
		t.Fatal("read rule should NOT allow write")
	}
}

func TestRuleForAndPersistence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WANCTL_CONFIG_DIR", dir)
	e, _ := Open("rules.json", ModeNormal)

	// Dir-scope exec with empty cwd downgrades to global.
	r := RuleFor(Request{Kind: KindExec, Cmd: "echo hi", Cwd: ""}, ScopeDir)
	if r.Scope != ScopeGlobal {
		t.Fatalf("expected downgrade to global, got %v", r.Scope)
	}
	e.Add(r)
	// Dir-scope file remembers the file's directory.
	rf := RuleFor(Request{Kind: KindWrite, Path: "/p/sub/f.txt"}, ScopeDir)
	if rf.Pattern != filepath.Clean("/p/sub") {
		t.Fatalf("file dir-scope pattern = %q", rf.Pattern)
	}
	e.Add(rf)

	// Reload from disk and confirm rules survived.
	e2, err := Open("rules.json", ModeNormal)
	if err != nil {
		t.Fatal(err)
	}
	if len(e2.List()) != 2 {
		t.Fatalf("expected 2 persisted rules, got %d", len(e2.List()))
	}
	if !e2.Allowed(Request{Kind: KindExec, Cmd: "echo hi there"}) {
		t.Fatal("persisted global exec rule should apply after reload")
	}
}
