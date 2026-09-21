package agent

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"wanctl/internal/client"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
)

type workspaceRights struct {
	mu                         sync.Mutex
	tokenRevoked, shareRevoked bool
}

func (s *workspaceRights) Resolve(token string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if token == "agent" {
		return "owner", true
	}
	return "shared", token == "controller" && !s.tokenRevoked
}
func (s *workspaceRights) ACLGrant(caller, owner, device string) (relay.Grant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return relay.Grant{}, caller == "shared" && owner == "owner" && !s.shareRevoked
}
func (s *workspaceRights) revoke(token bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if token {
		s.tokenRevoked = true
	} else {
		s.shareRevoked = true
	}
}

func reusableWorkspaceFixture(t *testing.T, tr string) (*Agent, *client.Client, client.WorkspaceRef, *workspaceRights, string) {
	t.Helper()
	rights := &workspaceRights{}
	r := relay.New(rights)
	r.SetACL(rights)
	srv := httptest.NewServer(r.Handler())
	t.Cleanup(srv.Close)
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	a, err := New(Options{RelayURL: srv.URL, Token: "agent", Name: "reuse", Transport: tr, Mode: policy.ModeBypass})
	if err != nil {
		t.Fatal(err)
	}
	cid, err := transport.IdentityFromSeed([]byte(strings.Repeat("r", 32)), "reuse-test")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.known.Add(cid.Fingerprint, "reuse-test"); err != nil {
		t.Fatal(err)
	}
	known := transport.NewMemStore()
	target := "owner/" + a.DeviceID()
	if err := known.Pin(target, a.id.Fingerprint, false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); a.Close() })
	go a.Run(ctx)
	owner := client.NewWith(cid, known, srv.URL, "agent", tr)
	for deadline := time.Now().Add(5 * time.Second); ; {
		peers, e := owner.Peers(ctx)
		if e == nil && len(peers) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent not registered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c := client.NewWith(cid, known, srv.URL, "controller", tr)
	link := client.NewWorkspaceLink()
	t.Cleanup(link.Close)
	c.UseWorkspaceLink(link)
	ref, err := c.PrepareWorkspace(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if _, err := c.Workspace(ctx, ref, "open", protocol.Message{Path: root}); err != nil {
		t.Fatal(err)
	}
	return a, c, ref, rights, root
}

func TestReusableWorkspaceRechecksAuthority(t *testing.T) {
	for _, tr := range []string{"ws", "http"} {
		for _, kind := range []string{"token", "share", "device-trust"} {
			t.Run(tr+"/"+kind, func(t *testing.T) {
				a, c, ref, rights, root := reusableWorkspaceFixture(t, tr)
				switch kind {
				case "token":
					rights.revoke(true)
				case "share":
					rights.revoke(false)
				case "device-trust":
					if err := a.known.Remove(c.Identity().Fingerprint); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, err := c.WriteFile(ctx, client.WriteRequest{Target: ref.Target, WorkspaceID: ref.ID, Path: "forbidden", Content: "must not write"})
				// HTTP authenticates each carrier upload, so revocation can be
				// refused as 401 before the encrypted request reaches the agent.
				denied := err != nil && (strings.Contains(err.Error(), "access inactive") || (tr == "http" && kind == "token" && strings.Contains(err.Error(), "401")))
				if !denied {
					t.Fatalf("revoked %s accepted or lost rejection: %v", kind, err)
				}
				if _, err := os.Stat(filepath.Join(root, "forbidden")); !os.IsNotExist(err) {
					t.Fatalf("revoked request changed file: %v", err)
				}
			})
		}
	}
}

func TestReusableWorkspaceRechecksAfterApproval(t *testing.T) {
	a, c, ref, rights, root := reusableWorkspaceFixture(t, "http")
	a.engine.SetMode(policy.ModeNormal)
	ap := workspaceHeldApproval{entered: make(chan struct{}), release: make(chan struct{})}
	a.setApprover(ap)
	type outcome struct {
		r   *protocol.WorkspaceResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		r, e := c.Workspace(context.Background(), ref, "exec", protocol.Message{RequestID: "pending-approval", Command: "printf unauthorized > forbidden"})
		done <- outcome{r, e}
	}()
	select {
	case <-ap.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("approval not requested")
	}
	rights.revoke(false)
	close(ap.release)
	select {
	case got := <-done:
		if got.err != nil || got.r == nil || !got.r.Done || got.r.Error == "" {
			t.Fatalf("approval result: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("approval result blocked")
	}
	if _, err := os.Stat(filepath.Join(root, "forbidden")); !os.IsNotExist(err) {
		t.Fatalf("late approval executed revoked request: %v", err)
	}
}

func TestReusableWorkspaceCancelDoesNotWaitForApprovalConnection(t *testing.T) {
	a, c, ref, _, root := reusableWorkspaceFixture(t, "http")
	a.engine.SetMode(policy.ModeNormal)
	ap := workspaceHeldApproval{entered: make(chan struct{}), release: make(chan struct{})}
	a.setApprover(ap)
	done := make(chan error, 1)
	go func() {
		_, err := c.Workspace(context.Background(), ref, "exec", protocol.Message{RequestID: "approval", Command: "printf forbidden > forbidden"})
		done <- err
	}()
	select {
	case <-ap.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("approval never requested")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, err := c.Workspace(ctx, ref, "cancel", protocol.Message{RequestID: "approval"})
	close(ap.release)
	if err != nil || r == nil || !r.Done || r.State != "invalid" || r.Error == "" {
		t.Fatalf("cancel blocked or incomplete: %+v %v", r, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "forbidden")); !os.IsNotExist(err) {
		t.Fatal("cancelled approval executed")
	}
}
