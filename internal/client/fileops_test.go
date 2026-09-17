package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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
)

// startDevice brings up a relay, an agent and a controller pinned to it, the
// same fixture the exec/push/pull round trip uses.
func startDevice(t *testing.T, mode policy.Mode) (*Client, context.Context) {
	t.Helper()
	return startDeviceWithRules(t, mode)
}

// startDeviceWithRules is startDevice with the device's rule file seeded before
// the agent reads it, for the cases where what is under test is a request the
// device would otherwise have to ask a human about.
func startDeviceWithRules(t *testing.T, mode policy.Mode, rules ...policy.Rule) (*Client, context.Context) {
	t.Helper()
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	t.Cleanup(srv.Close)
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	deviceConfig := t.TempDir()
	t.Setenv("WANCTL_CONFIG_DIR", deviceConfig)
	if len(rules) > 0 {
		encoded, err := json.Marshal(rules)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(deviceConfig, "rules.json"), encoded, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ag, err := agent.New(agent.Options{
		RelayURL: base, Token: "tok", Name: "home-pc", AutoYes: true, Mode: mode, Version: "v0.9.4-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ag.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go ag.Run(ctx)
	time.Sleep(200 * time.Millisecond)

	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", base)
	t.Setenv("WANCTL_TOKEN", "tok")
	t.Setenv("WANCTL_TRANSPORT", "ws")
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	trustServer(t, c, "home-pc")
	return c, ctx
}

// The whole point of the two verbs: a controller reads a range of a remote file,
// edits it against the hash it just saw, and reads back exactly what it wrote —
// over the real relay, through a real agent, with no shell anywhere in the path.
func TestReadEditRoundTrip(t *testing.T) {
	c, ctx := startDevice(t, policy.ModeBypass)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.ini")
	var sb strings.Builder
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&sb, "key%d = value%d\n", i, i)
	}
	if err := os.WriteFile(path, []byte(sb.String()), 0o640); err != nil {
		t.Fatal(err)
	}

	res, err := c.ReadFile(ctx, ReadRequest{Target: "home-pc", Path: path, Offset: 100, Limit: 3})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if res.Content != "key100 = value100\nkey101 = value101\nkey102 = value102\n" {
		t.Fatalf("content = %q", res.Content)
	}
	if res.FirstLine != 100 || res.LastLine != 102 || res.TotalLines != 300 || res.Truncated {
		t.Fatalf("result = %+v", res)
	}

	edit, err := c.EditFile(ctx, EditRequest{
		Target: "home-pc", Path: path,
		Old: "key101 = value101", New: "key101 = CHANGED", ExpectedSHA: res.SHA256,
	})
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if edit.Replaced != 1 {
		t.Fatalf("replaced = %d, want 1", edit.Replaced)
	}

	back, err := c.ReadFile(ctx, ReadRequest{Target: "home-pc", Path: path, Offset: 101, Limit: 1})
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if back.Content != "key101 = CHANGED\n" {
		t.Fatalf("read back = %q", back.Content)
	}
	if back.SHA256 != edit.SHA256 {
		t.Fatalf("hash after edit = %s, read back %s", edit.SHA256, back.SHA256)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("mode after edit = %v (err=%v), want 0640", info.Mode().Perm(), err)
	}

	// A stale hash is refused end to end, and the refusal carries the current
	// one so the caller can recover without guessing.
	_, err = c.EditFile(ctx, EditRequest{
		Target: "home-pc", Path: path, Old: "key102", New: "nope", ExpectedSHA: res.SHA256,
	})
	var refused *FileOpError
	if !errors.As(err, &refused) {
		t.Fatalf("stale edit error = %v, want a FileOpError", err)
	}
	if refused.Result == nil || refused.Result.SHA256 != edit.SHA256 {
		t.Fatalf("refusal did not carry the current hash: %+v", refused.Result)
	}
}

// A controller must not send a relative path and let the device decide what it
// meant; that is a different file on every device.
func TestFileOpsRequireAnAbsolutePath(t *testing.T) {
	c := &Client{}
	if _, err := c.ReadFile(context.Background(), ReadRequest{Target: "x", Path: "~/notes.txt"}); err == nil ||
		!strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("error = %v, want the absolute-path refusal", err)
	}
	if _, err := c.EditFile(context.Background(), EditRequest{Target: "x", Path: "rel.txt", Old: "a", New: "b"}); err == nil ||
		!strings.Contains(err.Error(), "must be absolute") {
		t.Fatalf("error = %v, want the absolute-path refusal", err)
	}
}

// A device running an agent from before these verbs existed answers the way the
// request loop's default branch does: one error naming the unknown kind, then
// the session ends. The controller must turn that into the one instruction that
// fixes it rather than passing the wire text along -- and must do so promptly,
// since an operator staring at a hung terminal is the failure this replaces.
func TestOldAgentUnknownKindBecomesAnUpdateInstruction(t *testing.T) {
	device, controller := net.Pipe()
	defer controller.Close()
	go func() {
		defer device.Close()
		if _, err := protocol.ReadMessage(device); err != nil {
			return
		}
		// Verbatim from internal/agent/agent.go's serveAuthorized default
		// branch: an error naming the kind, and then the loop returns.
		protocol.WriteMessage(device, protocol.Message{
			Kind: protocol.KindError, Reason: "unknown request: " + protocol.KindFileRead,
		})
	}()

	start := time.Now()
	_, err := fileOpOver(controller, protocol.Message{Kind: protocol.KindFileRead, Path: "/etc/hosts"})
	elapsed := time.Since(start)

	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("error = %v, want an UnsupportedError", err)
	}
	if !strings.Contains(err.Error(), "does not support read/edit") || !strings.Contains(err.Error(), "wanctl update") {
		t.Fatalf("message = %q, want the update instruction", err.Error())
	}
	if elapsed > 10*time.Second {
		t.Fatalf("took %v to report an unsupported agent", elapsed)
	}
}

// The same thing against a live agent, so the claim above rests on what the
// agent actually does with a kind it does not know rather than on a stub that
// imitates it. The kind is one no agent will ever implement; today's agent
// treats file_read the same way any agent older than this change does.
func TestLiveAgentUnknownKindBecomesAnUpdateInstruction(t *testing.T) {
	c, ctx := startDevice(t, policy.ModeBypass)

	conn, err := c.connect(ctx, "home-pc")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	start := time.Now()
	_, err = fileOpOver(conn, protocol.Message{Kind: "file_read_from_a_later_version", Path: "/etc/hosts"})
	elapsed := time.Since(start)

	var unsupported *UnsupportedError
	if !errors.As(err, &unsupported) {
		t.Fatalf("error = %v, want an UnsupportedError", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("took %v to report an unsupported agent", elapsed)
	}
}

// Naming the device's version makes the instruction actionable, and it costs a
// second dial on a path that has already failed, so it must not be what makes
// the caller wait: the lookup has its own budget and its failure is silent.
func TestUnsupportedErrorNamesTheAgentVersion(t *testing.T) {
	c, ctx := startDevice(t, policy.ModeBypass)

	unsupported := &UnsupportedError{Target: "home-pc", Kind: protocol.KindFileRead}
	unsupported.Version = c.agentVersion(ctx, "home-pc")
	if unsupported.Version != "v0.9.4-test" {
		t.Fatalf("version = %q, want v0.9.4-test", unsupported.Version)
	}
	if !strings.Contains(unsupported.Error(), "device agent v0.9.4-test does not support read/edit") {
		t.Fatalf("message = %q", unsupported.Error())
	}
}
