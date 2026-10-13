package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"wanctl/internal/eventlog"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
)

func workspaceAgent(t *testing.T) *Agent {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	a, err := New(Options{RelayURL: "http://unused", Token: "synthetic", Mode: policy.ModeNormal, Shell: "/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}

// runInWorkspace submits one command and waits for its result.
func runInWorkspace(t *testing.T, a *Agent, w *workspace, id, command string) *protocol.WorkspaceResult {
	t.Helper()
	if err := a.startWorkspaceCommand(w, "controller", "review", protocol.Message{Action: "exec", RequestID: id, Command: command}, sessionAudit{}); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		w.wait(id, 1000)
		if r, err := w.snapshot(id, 0); err != nil {
			t.Fatal(err)
		} else if r.Done {
			return r
		}
	}
	t.Fatalf("request %s did not finish", id)
	return nil
}

func quoteSh(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// A command sent without a cwd runs wherever the workspace shell is now. A
// directory-scoped rule is a statement about the directory a command runs in,
// so it has to be matched against that directory — not against the root the
// workspace happened to be opened on before an earlier command moved it.
func TestWorkspaceDirectoryRuleFollowsTheShell(t *testing.T) {
	a := workspaceAgent(t)
	allowed, outside := t.TempDir(), t.TempDir()
	for dir, text := range map[string]string{allowed: "allowed-marker", outside: "outside-marker"} {
		if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w, err := a.openWorkspace("controller", protocol.Message{WorkspaceID: "w-" + strings.Repeat("a", 32), Path: allowed})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.engine.Add(policy.Rule{Kind: policy.KindExec, Pattern: "cat marker.txt", Scope: policy.ScopeDir, Dir: allowed}); err != nil {
		t.Fatal(err)
	}
	a.setApprover(policy.DenyApprover{})

	// Before anything moves the shell, the rule for the root still applies.
	if r := runInWorkspace(t, a, w, "in-root", "cat marker.txt"); r.Code != 0 || strings.TrimSpace(r.Output) != "allowed-marker" {
		t.Fatalf("rule for the root no longer covers the root: %+v", r)
	}

	a.setApprover(policy.AllowApprover{})
	if r := runInWorkspace(t, a, w, "move", "cd "+quoteSh(outside)); r.Code != 0 {
		t.Fatalf("cd: %+v", r)
	}
	a.setApprover(policy.DenyApprover{})
	r := runInWorkspace(t, a, w, "elsewhere", "cat marker.txt")
	if strings.Contains(r.Output, "outside-marker") {
		t.Fatal("a rule for the workspace root authorized a command the shell ran in another directory")
	}
	if r.Error == "" || !strings.Contains(r.Error, "denied") {
		t.Fatalf("want a policy refusal, got %+v", r)
	}

	// The audit trail says where the command would have run.
	events, err := a.log.Read(eventlog.Filter{Type: "exec", Grep: "request elsewhere"})
	if err != nil || len(events) == 0 {
		t.Fatalf("no audit event: %v", err)
	}
	if got := events[len(events)-1].Cwd; got != outside {
		t.Fatalf("audit cwd = %q, want the shell's directory %q", got, outside)
	}
}

// The same holds in the other direction: a rule for the directory the shell has
// moved into applies there, without the caller having to repeat the cwd.
func TestWorkspaceDirectoryRuleAppliesWhereTheShellMoved(t *testing.T) {
	a := workspaceAgent(t)
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "marker.txt"), []byte("sub-marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := a.openWorkspace("controller", protocol.Message{WorkspaceID: "w-" + strings.Repeat("b", 32), Path: root})
	if err != nil {
		t.Fatal(err)
	}
	a.setApprover(policy.AllowApprover{})
	if r := runInWorkspace(t, a, w, "move", "cd sub"); r.Code != 0 {
		t.Fatalf("cd: %+v", r)
	}
	if err := a.engine.Add(policy.Rule{Kind: policy.KindExec, Pattern: "cat marker.txt", Scope: policy.ScopeDir, Dir: sub}); err != nil {
		t.Fatal(err)
	}
	a.setApprover(policy.DenyApprover{})
	if r := runInWorkspace(t, a, w, "read", "cat marker.txt"); r.Code != 0 || strings.TrimSpace(r.Output) != "sub-marker" {
		t.Fatalf("rule for the shell's directory did not apply: %+v", r)
	}
}

// A finished command reports where it left the shell, so a caller showing the
// directory does not need a second command to learn it.
func TestWorkspaceResultReportsWhereTheShellStopped(t *testing.T) {
	a := workspaceAgent(t)
	a.setApprover(policy.AllowApprover{})
	root, next := t.TempDir(), t.TempDir()
	w, err := a.openWorkspace("controller", protocol.Message{WorkspaceID: "w-" + strings.Repeat("c", 32), Path: root})
	if err != nil {
		t.Fatal(err)
	}
	same := func(want, got string) bool {
		a, errA := filepath.EvalSymlinks(want)
		b, errB := filepath.EvalSymlinks(got)
		return errA == nil && errB == nil && a == b
	}
	if r := runInWorkspace(t, a, w, "stay", "true"); !same(root, r.Cwd) {
		t.Fatalf("cwd after a command that did not move = %q, want %q", r.Cwd, root)
	}
	if r := runInWorkspace(t, a, w, "move", "cd "+quoteSh(next)); !same(next, r.Cwd) {
		t.Fatalf("cwd after cd = %q, want %q", r.Cwd, next)
	}
}
