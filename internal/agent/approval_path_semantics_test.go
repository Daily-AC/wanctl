package agent

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/server"
)

// The gate and the persistent shell must agree on symlink/.. semantics.
func TestLateGrantSymlinkParentStaysInApprovedDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell path semantics")
	}
	for _, oneshot := range []bool{false, true} {
		t.Run(map[bool]string{false: "persistent", true: "oneshot"}[oneshot], func(t *testing.T) {
			root := canonicalApprovalDir(t.TempDir())
			approved := filepath.Join(root, "physical", "work")
			actual := filepath.Join(root, "logical", "work")
			for _, dir := range []string{approved, actual, filepath.Join(root, "physical", "leaf")} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			link := filepath.Join(root, "logical", "link")
			if err := os.Symlink(filepath.Join(root, "physical", "leaf"), link); err != nil {
				t.Fatal(err)
			}
			// Do not filepath.Join: it would erase the meaningful link/.. component.
			retryDir := link + "/../work"
			a, _ := newGrantAgent(t)
			a.opts.Shell = "/bin/sh"
			req := policy.Request{Kind: policy.KindExec, Cmd: "pwd -P", Cwd: approved, Peer: testFP(1)}
			if ok, _ := a.gate(req); ok {
				t.Fatal("original request unexpectedly allowed")
			}
			res := a.handleConsoleRPC(protocol.Message{Kind: protocol.KindGrantOnce, RuleKind: "exec", Pattern: req.Cmd,
				FP: req.Peer, Cwd: approved, Approver: "phone:test"})
			if res.Kind != protocol.KindGrantOnce {
				t.Fatal(res)
			}
			req.Cwd = retryDir
			allowed, decision := a.gate(req)
			if !allowed {
				req.Cwd = approved
				if ok, why := a.gate(req); !ok || why != "late-approved" {
					t.Fatalf("original grant lost: %v %s", ok, why)
				}
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var out bytes.Buffer
			var code int
			var err, sessionErr error
			if oneshot {
				code, err = server.RunOneShotContext(ctx, a.opts.Shell, req.Cmd, req.Cwd, &out)
			} else {
				code, err, sessionErr = a.execInSession(ctx, req.Peer, protocol.Message{Command: req.Cmd, Cwd: req.Cwd}, &out)
			}
			if err != nil || sessionErr != nil || code != 0 {
				t.Fatalf("execute: code=%d err=%v sessionErr=%v output=%q", code, err, sessionErr, out.String())
			}
			if got := strings.TrimSpace(out.String()); got != approved {
				t.Fatalf("%s authorized cwd %q, but persistent shell ran in %q (retry cwd %q)", decision, approved, got, retryDir)
			}
		})
	}
}

func TestLateGrantDirectoryForms(t *testing.T) {
	root := canonicalApprovalDir(t.TempDir())
	real, alias := filepath.Join(root, "real"), filepath.Join(root, "alias")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, dir string
		allowed   bool
	}{
		{"canonical", real, true}, {"symlink", alias, true}, {"dot", real + "/.", true},
		{"relative", "real", false}, {"empty", "", false}, {"missing", root + "/missing", false},
		{"different", root, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := newGrantAgent(t)
			res := a.handleConsoleRPC(protocol.Message{Kind: protocol.KindGrantOnce, RuleKind: "exec", Pattern: "pwd -P",
				FP: testFP(1), Cwd: real, Approver: "phone:test"})
			if res.Kind != protocol.KindGrantOnce {
				t.Fatal(res)
			}
			ok, why := a.gate(policy.Request{Kind: policy.KindExec, Cmd: "pwd -P", Cwd: tc.dir, Peer: testFP(1)})
			if ok != tc.allowed {
				t.Fatalf("cwd %q: allowed=%v (%s), want %v", tc.dir, ok, why, tc.allowed)
			}
		})
	}
}
