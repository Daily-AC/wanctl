package client

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wanctl/internal/agent"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/relay"
	"wanctl/internal/transport"
)

func workspaceFixture(t *testing.T, tr string) (*Client, *agent.Agent) {
	t.Helper()
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	t.Cleanup(srv.Close)
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	ag, err := agent.New(agent.Options{RelayURL: srv.URL, Token: "tok", Name: "home-pc", AutoYes: true, Transport: tr, Mode: policy.ModeBypass})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); ag.Close() })
	go ag.Run(ctx)
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", srv.URL)
	t.Setenv("WANCTL_TOKEN", "tok")
	t.Setenv("WANCTL_TRANSPORT", tr)
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		peers, err := c.Peers(ctx)
		if err == nil && len(peers) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("device did not register: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	trustServer(t, c, "home-pc")
	return c, ag
}

func openTestWorkspace(t *testing.T, c *Client, root string) WorkspaceRef {
	t.Helper()
	ref, err := c.PrepareWorkspace(context.Background(), "home-pc")
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Workspace(context.Background(), ref, "open", protocol.Message{Path: root})
	if err != nil || r.State != "open" {
		t.Fatalf("open: %+v %v", r, err)
	}
	return ref
}

func awaitWorkspace(t *testing.T, c *Client, ref WorkspaceRef, id string) *protocol.WorkspaceResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		r, err := c.Workspace(ctx, ref, "poll", protocol.Message{RequestID: id})
		if err != nil {
			t.Fatal(err)
		}
		if r.Done {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func runWorkspace(t *testing.T, c *Client, ref WorkspaceRef, id, command string) *protocol.WorkspaceResult {
	t.Helper()
	_, err := c.Workspace(context.Background(), ref, "exec", protocol.Message{RequestID: id, Command: command})
	if err != nil {
		t.Fatal(err)
	}
	r := awaitWorkspace(t, c, ref, id)
	if r.Code != 0 || r.Error != "" {
		t.Fatalf("exec %s: %+v", id, r)
	}
	return r
}

// Actual relay transports, TLS identities, device policy, files and OS shell;
// no fake command runner. Every API invocation creates a new connection.
func TestWorkspaceEndToEnd(t *testing.T) {
	for _, tr := range []string{"ws", "http"} {
		t.Run(tr, func(t *testing.T) {
			c, ag := workspaceFixture(t, tr)
			rootA, rootB := t.TempDir(), t.TempDir()
			a, b := openTestWorkspace(t, c, rootA), openTestWorkspace(t, c, rootB)
			if a.ID == b.ID || !ag.Busy() {
				t.Fatal("workspaces not independently owned")
			}
			runWorkspace(t, c, a, "env", "export WANCTL_LESSON=alpha; mkdir child; cd child")
			r := runWorkspace(t, c, a, "persist", "printf '%s\n' \"$WANCTL_LESSON\"; pwd")
			if !strings.Contains(r.Output, "alpha") || !strings.Contains(r.Output, "child") {
				t.Fatalf("state did not persist: %+v", r)
			}
			r = runWorkspace(t, c, b, "isolated", "printf '%s\n' \"${WANCTL_LESSON-unset}\"; pwd")
			realB, _ := filepath.EvalSymlinks(rootB)
			if !strings.Contains(r.Output, "unset") || (!strings.Contains(r.Output, realB) && !strings.Contains(r.Output, rootB)) {
				t.Fatalf("workspace bleed: %+v", r)
			}
			if _, err := c.WriteFile(context.Background(), WriteRequest{Target: a.Target, WorkspaceID: a.ID, Path: "lesson.txt", Content: "before\n"}); err != nil {
				t.Fatal(err)
			}
			read, err := c.ReadFile(context.Background(), ReadRequest{Target: a.Target, WorkspaceID: a.ID, Path: "lesson.txt"})
			if err != nil || read.Content != "before\n" {
				t.Fatalf("read: %+v %v", read, err)
			}
			if _, err := c.EditFile(context.Background(), EditRequest{Target: a.Target, WorkspaceID: a.ID, Path: "lesson.txt", Old: "before", New: "after", ExpectedSHA: read.SHA256}); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(rootA, "lesson.txt"))
			if err != nil || string(got) != "after\n" {
				t.Fatalf("wrong actual file: %q %v", got, err)
			}
			if _, err := c.EditFile(context.Background(), EditRequest{Target: a.Target, WorkspaceID: a.ID, Path: "lesson.txt", Old: "after", New: "bad", ExpectedSHA: read.SHA256}); err == nil {
				t.Fatal("lost hash conflict protection")
			}

			// Deliver the request and deliberately abandon the TLS connection
			// without reading its acknowledgement. Recover by ID on another one.
			conn, err := c.connect(context.Background(), a.Target)
			if err != nil {
				t.Fatal(err)
			}
			msg := protocol.Message{Kind: protocol.KindWorkspace, Action: "exec", WorkspaceID: a.ID, RequestID: "lost-reply", Command: "printf x >> once; sleep 0.2; printf recovered"}
			if err := protocol.WriteMessage(conn, msg); err != nil {
				t.Fatal(err)
			}
			conn.Close()
			deadline := time.Now().Add(5 * time.Second)
			for {
				r, err = c.Workspace(context.Background(), a, "exec", msg)
				if err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal(err)
				}
				time.Sleep(10 * time.Millisecond)
			}
			r = awaitWorkspace(t, c, a, "lost-reply")
			if r.Code != 0 || !strings.Contains(r.Output, "recovered") {
				t.Fatalf("recovery: %+v", r)
			}
			got, _ = os.ReadFile(filepath.Join(rootA, "child", "once"))
			if string(got) != "x" {
				t.Fatalf("request ran more than once: %q", got)
			}
			msg.Command = "printf second"
			if _, err := c.Workspace(context.Background(), a, "exec", msg); err == nil || !strings.Contains(err.Error(), "conflict") {
				t.Fatalf("ID conflict accepted: %v", err)
			}

			// A separately authenticated controller cannot use another one's ID.
			otherID, err := transport.IdentityFromSeed([]byte(strings.Repeat("b", 32)), "other")
			if err != nil {
				t.Fatal(err)
			}
			other := NewWith(otherID, c.known, c.relayURL, c.token, c.transport)
			if _, err := other.Workspace(context.Background(), a, "status", protocol.Message{}); err == nil {
				t.Fatal("cross-controller workspace access")
			}
			if _, err := c.Workspace(context.Background(), a, "close", protocol.Message{}); err != nil {
				t.Fatal(err)
			}
			if _, err := c.ReadFile(context.Background(), ReadRequest{Target: a.Target, WorkspaceID: a.ID, Path: "lesson.txt"}); err == nil {
				t.Fatal("closed workspace fell back")
			}
			if _, err := c.Workspace(context.Background(), b, "status", protocol.Message{}); err != nil {
				t.Fatalf("closing A affected B: %v", err)
			}
			if _, err := c.Workspace(context.Background(), b, "close", protocol.Message{}); err != nil {
				t.Fatal(err)
			}
			if ag.Busy() {
				t.Fatal("closed workspaces keep updater busy")
			}
		})
	}
}

func TestWorkspaceCancelKeepsLedgerButInvalidatesShell(t *testing.T) {
	c, _ := workspaceFixture(t, "ws")
	ref := openTestWorkspace(t, c, t.TempDir())
	_, err := c.Workspace(context.Background(), ref, "exec", protocol.Message{RequestID: "long", Command: "sleep 30; printf should-not-run"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Workspace(context.Background(), ref, "exec", protocol.Message{RequestID: "other", Command: "true"}); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("busy=%v", err)
	}
	r, err := c.Workspace(context.Background(), ref, "cancel", protocol.Message{RequestID: "long"})
	if err != nil || !r.Done || r.State != "invalid" || r.Error == "" {
		t.Fatalf("cancel=%+v %v", r, err)
	}
	if strings.Contains(r.Output, "should-not-run") {
		t.Fatal("cancel continued remainder")
	}
	if _, err := c.Workspace(context.Background(), ref, "exec", protocol.Message{RequestID: "new", Command: "true"}); err == nil {
		t.Fatal("silently recreated lost shell")
	}
}

func TestWorkspaceOldAgentNeverRunsLegacyFileOperation(t *testing.T) {
	// This is the exact response an old agent sends for the new envelope.
	rw := &workspaceOldPeer{}
	_, err := fileOpOver(rw, protocol.Message{Kind: protocol.KindWorkspace, Action: protocol.KindFileRead, Path: "relative.txt", WorkspaceID: "w-" + strings.Repeat("a", 32)})
	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) || unsupported.Kind != protocol.KindWorkspace {
		t.Fatalf("old agent: %T %v", err, err)
	}
}

type workspaceOldPeer struct{ data []byte }

func (p *workspaceOldPeer) Write(b []byte) (int, error) {
	if p.data == nil {
		var s strings.Builder
		protocol.WriteMessage(&s, protocol.Message{Kind: protocol.KindError, Reason: "unknown request: workspace"})
		p.data = []byte(s.String())
	}
	return len(b), nil
}
func (p *workspaceOldPeer) Read(b []byte) (int, error) {
	n := copy(b, p.data)
	p.data = p.data[n:]
	return n, nil
}
