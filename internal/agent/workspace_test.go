package agent

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wanctl/internal/client"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/relay"
)

func TestWorkspaceOperationsUseExistingPolicy(t *testing.T) {
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")
	ap := requestKindApprover{kinds: make(chan policy.Kind, 8)}
	ag := startAgent(t, base, ap, policy.ModeNormal)
	dr := connectController(t, base)
	defer dr.Conn.Close()
	root := t.TempDir()
	id := "w-" + strings.Repeat("c", 32)
	reply := fileOpReply(t, dr, protocol.Message{Kind: protocol.KindWorkspace, Action: "open", WorkspaceID: id, Path: root})
	if reply.Workspace == nil {
		t.Fatalf("open: %+v", reply)
	}
	reply = fileOpReply(t, dr, protocol.Message{Kind: protocol.KindWorkspace, Action: protocol.KindFileWrite, WorkspaceID: id, Path: "test.txt", Content: "allowed"})
	if reply.Kind != protocol.KindFileResult {
		t.Fatalf("write: %+v", reply)
	}
	reply = fileOpReply(t, dr, protocol.Message{Kind: protocol.KindWorkspace, Action: protocol.KindFileRead, WorkspaceID: id, Path: "test.txt"})
	if reply.Kind != protocol.KindFileResult {
		t.Fatalf("read: %+v", reply)
	}
	reply = fileOpReply(t, dr, protocol.Message{Kind: protocol.KindWorkspace, Action: "exec", WorkspaceID: id, RequestID: "allowed", Command: "printf allowed"})
	if reply.Workspace == nil {
		t.Fatalf("exec: %+v", reply)
	}
	for _, want := range []policy.Kind{policy.KindRead, policy.KindWrite, policy.KindRead, policy.KindExec} {
		select {
		case got := <-ap.kinds:
			if got != want {
				t.Fatalf("gate %s instead of %s", got, want)
			}
		default:
			t.Fatalf("missing %s gate", want)
		}
	}
	ag.setApprover(policy.DenyApprover{})
	reply = fileOpReply(t, dr, protocol.Message{Kind: protocol.KindWorkspace, Action: protocol.KindFileWrite, WorkspaceID: id, Path: "test.txt", Content: "denied"})
	if reply.Kind != protocol.KindReject {
		t.Fatalf("write bypassed policy: %+v", reply)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "test.txt")); string(b) != "allowed" {
		t.Fatalf("denied write changed file: %s", b)
	}
}

func TestDelegatedGrantCannotOpenPersistentWorkspace(t *testing.T) {
	f := startDelegationFixture(t, "http", "http", policy.ModeBypass, true, time.Minute)
	ref, err := f.c.PrepareWorkspace(f.ctx, f.target)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.c.Workspace(f.ctx, ref, "open", protocol.Message{Path: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "synchronous one-shot") {
		t.Fatalf("delegation widened: %v", err)
	}
	if f.a.workspacesBusy() {
		t.Fatal("refused delegated request left a workspace")
	}
}

func TestWorkspaceOutputAndLedgerAreBounded(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	a, err := New(Options{RelayURL: "http://unused", Token: "t", Mode: policy.ModeBypass})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	w, err := a.openWorkspace("owner", protocol.Message{WorkspaceID: "w-" + strings.Repeat("d", 32), Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := w.reserve(protocol.Message{RequestID: "bounded", Command: "echo"})
	if err != nil || !fresh {
		t.Fatal(err)
	}
	large := bytes.Repeat([]byte("x"), maxWorkspaceOutput+50)
	if n, err := (workspaceWriter{w, "bounded"}).Write(large); err != nil || n != len(large) {
		t.Fatalf("write: %d %v", n, err)
	}
	r, err := w.snapshot("bounded", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Truncated || r.RetainedBytes != maxWorkspaceOutput || r.OutputBytes != int64(len(large)) || len(r.Output) != workspaceOutputPage {
		t.Fatalf("bounds: %+v", r)
	}
	w.mu.Lock()
	w.finishLocked("bounded", 0, nil)
	w.mu.Unlock()
	for i := 1; i < maxWorkspaceRequests; i++ {
		id := client.NewRequestID()
		if _, err := w.reserve(protocol.Message{RequestID: id, Command: "true"}); err != nil {
			t.Fatal(err)
		}
		w.mu.Lock()
		w.finishLocked(id, 0, nil)
		w.mu.Unlock()
	}
	if _, err := w.reserve(protocol.Message{RequestID: "overflow", Command: "true"}); err == nil {
		t.Fatal("unbounded request ledger")
	}
	if fresh, err := w.reserve(protocol.Message{RequestID: "bounded", Command: "echo"}); err != nil || fresh {
		t.Fatalf("full ledger lost deduplication: %v %v", fresh, err)
	}
}

func TestWorkspaceCloseDuringApprovalDoesNotExecute(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	a, err := New(Options{RelayURL: "http://unused", Token: "t", Mode: policy.ModeNormal})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	root := t.TempDir()
	w, err := a.openWorkspace("owner", protocol.Message{WorkspaceID: "w-" + strings.Repeat("e", 32), Path: root})
	if err != nil {
		t.Fatal(err)
	}
	ap := workspaceHeldApproval{entered: make(chan struct{}), release: make(chan struct{})}
	a.setApprover(ap)
	done := make(chan error, 1)
	go func() {
		done <- a.startWorkspaceCommand(w, "owner", "tester", protocol.Message{RequestID: "waiting", Command: "printf bad > should-not-exist"}, sessionAudit{})
	}()
	select {
	case <-ap.entered:
	case <-time.After(time.Second):
		t.Fatal("approval never requested")
	}
	w.stop()
	close(ap.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatalf("ran after close: %v", err)
	}
	r, err := w.snapshot("waiting", 0)
	if err != nil || !r.Done || r.Error == "" {
		t.Fatalf("lost refusal: %+v %v", r, err)
	}
}

func TestWorkspaceOutputPreservesUnicodeAcrossPagesAndWrites(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	a, err := New(Options{RelayURL: "http://unused", Token: "t", Mode: policy.ModeBypass})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	w, err := a.openWorkspace("owner", protocol.Message{WorkspaceID: "w-" + strings.Repeat("f", 32), Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.reserve(protocol.Message{RequestID: "unicode", Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	want := strings.Repeat("a", workspaceOutputPage-1) + "中文结果"
	writer := workspaceWriter{w, "unicode"}
	writer.Write([]byte(want))
	r, err := w.snapshot("unicode", 0)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := w.snapshot("unicode", r.NextOffset)
	if err != nil {
		t.Fatal(err)
	}
	if r.Output+r2.Output != want {
		t.Fatal("split a Unicode character between pages")
	}
	w.mu.Lock()
	w.finishLocked("unicode", 0, nil)
	w.mu.Unlock()
	if _, err := w.reserve(protocol.Message{RequestID: "partial", Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	writer = workspaceWriter{w, "partial"}
	encoded := []byte("中")
	writer.Write(encoded[:1])
	r, err = w.snapshot("partial", 0)
	if err != nil {
		t.Fatal(err)
	}
	if r.Output != "" || r.NextOffset != 0 {
		t.Fatal("returned an incomplete UTF-8 character")
	}
	writer.Write(encoded[1:])
	r, err = w.snapshot("partial", r.NextOffset)
	if err != nil {
		t.Fatal(err)
	}
	if r.Output != "中" {
		t.Fatalf("lost split write: %q", r.Output)
	}
}

type workspaceHeldApproval struct{ entered, release chan struct{} }

func (a workspaceHeldApproval) Ask(policy.Request) policy.Decision {
	close(a.entered)
	<-a.release
	return policy.Decision{Allow: true}
}
