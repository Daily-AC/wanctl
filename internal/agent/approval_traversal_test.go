package agent

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"wanctl/internal/policy"
	"wanctl/internal/protocol"
)

// Establish the executor's directory with ordinary approval before installing
// any grant. The expectation is independent of the approval path resolver.
func TestLateGrantDirectoryTraversalAcrossExecutors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell path semantics")
	}
	for _, traversal := range []string{"safe parent", "divergent parent"} {
		for _, mode := range []string{"persistent", "oneshot", "workspace"} {
			t.Run(traversal+"/"+mode, func(t *testing.T) {
				root, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				approved := filepath.Join(root, "physical", "repo")
				logical := filepath.Join(root, "logical", "repo")
				for _, dir := range []string{approved, logical, filepath.Join(root, "physical", "spare"), filepath.Join(root, "physical", "leaf")} {
					if err := os.MkdirAll(dir, 0700); err != nil {
						t.Fatal(err)
					}
				}
				home := filepath.Join(root, "home-link")
				door := filepath.Join(root, "logical", "link")
				if err := os.Symlink(filepath.Join(root, "physical"), home); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(root, "physical", "leaf"), door); err != nil {
					t.Fatal(err)
				}
				candidate := home + "/spare/../repo"
				actual := approved
				if traversal == "divergent parent" {
					candidate = door + "/../repo"
					if mode != "oneshot" {
						actual = logical
					}
				}
				a := newOptsAgent(t, Options{Shell: "/bin/sh"})
				a.setApprover(policy.AllowApprover{})
				conn := pipeController(t, a, 71)
				fp := seededIdentity(t, 71).Fingerprint
				workspaceID := "w-" + strings.Repeat("a", 32)
				if mode == "workspace" {
					reply := roundTrip(t, conn, protocol.Message{Kind: protocol.KindWorkspace, Action: "open", WorkspaceID: workspaceID, Path: root})
					if reply.Workspace == nil {
						t.Fatalf("open: %+v", reply)
					}
				}
				run := func(id, dir string) (string, bool) {
					t.Helper()
					if mode != "workspace" {
						return traversalExec(t, conn, dir, mode == "oneshot")
					}
					msg := protocol.Message{Kind: protocol.KindWorkspace, Action: "exec", WorkspaceID: workspaceID, RequestID: id, Command: "pwd -P", Cwd: dir, WaitMillis: 1000}
					for attempt := 0; attempt < 6; attempt++ {
						reply := roundTrip(t, conn, msg)
						if reply.Workspace == nil {
							t.Fatalf("workspace exec: %+v", reply)
						}
						r := reply.Workspace
						if r.Done {
							return strings.TrimSpace(r.Output), r.Code == 0 && r.Error == ""
						}
						msg = protocol.Message{Kind: protocol.KindWorkspace, Action: "poll", WorkspaceID: workspaceID, RequestID: id, WaitMillis: 1000}
					}
					t.Fatal("workspace command did not finish")
					return "", false
				}
				if got, ok := run("ordinary", candidate); !ok || got != actual {
					t.Fatalf("ordinary execution: directory %q, success=%v; want %q", got, ok, actual)
				}
				a.setApprover(policy.DenyApprover{})
				grant := a.handleConsoleRPC(protocol.Message{Kind: protocol.KindGrantOnce, RuleKind: "exec", Pattern: "pwd -P", FP: fp, Cwd: approved, Approver: "phone:test"})
				if grant.Kind != protocol.KindGrantOnce {
					t.Fatal(grant)
				}
				got, ok := run("retry", candidate)
				if traversal == "safe parent" {
					if !ok || got != approved {
						t.Fatalf("safe traversal lost its late grant: directory %q, success=%v; want %q", got, ok, approved)
					}
				} else {
					if ok || got != "" {
						t.Fatalf("divergent traversal executed: directory %q, success=%v", got, ok)
					}
					if got, ok = run("canonical", approved); !ok || got != approved {
						t.Fatalf("refused traversal consumed the grant: %q %v", got, ok)
					}
				}
				if _, ok := run("spent", approved); ok {
					t.Fatal("grant was consumed more than once")
				}
			})
		}
	}
}

func traversalExec(t *testing.T, conn *tls.Conn, cwd string, oneshot bool) (string, bool) {
	t.Helper()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := protocol.WriteMessage(conn, protocol.Message{Kind: protocol.KindExec, Command: "pwd -P", Cwd: cwd, OneShot: oneshot}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for {
		kind, payload, err := protocol.ReadFrame(conn)
		if err != nil {
			t.Fatal(err)
		}
		if kind == protocol.FrameStdout {
			out.Write(payload)
		}
		if kind == protocol.FrameJSON {
			msg, err := protocol.DecodeMessage(payload)
			if err != nil {
				t.Fatal(err)
			}
			switch msg.Kind {
			case protocol.KindExit:
				return strings.TrimSpace(out.String()), msg.Code == 0
			case protocol.KindReject:
				return strings.TrimSpace(out.String()), false
			case protocol.KindError:
				t.Fatalf("execution failed: %s", msg.Reason)
			}
		}
	}
}
