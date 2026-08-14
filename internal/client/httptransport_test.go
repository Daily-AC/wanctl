package client

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wanctl/internal/agent"
	"wanctl/internal/policy"
	"wanctl/internal/relay"
)

// Exercises the proxy-agnostic HTTP transport end-to-end: agent long-polls,
// controller dials over HTTP, exec + file round trip through io.Pipe relay.
func TestHTTPTransportExecAndFileRoundTrip(t *testing.T) {
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	base := srv.URL // http://...

	// Agent over HTTP transport.
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	ag, err := agent.New(agent.Options{RelayURL: base, Token: "tok", Name: "home-pc", AutoYes: true, Transport: "http", Mode: policy.ModeBypass})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)
	time.Sleep(300 * time.Millisecond) // let the first long-poll register the device

	// Controller over HTTP transport.
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", base)
	t.Setenv("WANCTL_TOKEN", "tok")
	t.Setenv("WANCTL_TRANSPORT", "http")
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}

	devs, err := c.Peers(context.Background())
	if err != nil || len(devs) != 1 || devs[0] != "home-pc" {
		t.Fatalf("peers: %v err=%v", devs, err)
	}
	trustServer(t, c, "home-pc")

	code, err := c.Exec(context.Background(), ExecRequest{Target: "home-pc", Command: "echo http-hi", OneShot: true, Cwd: ""})
	if err != nil || code != 0 {
		t.Fatalf("exec: code=%d err=%v", code, err)
	}

	local := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(local, []byte("http-payload-456"), 0o644)
	remote := filepath.Join(t.TempDir(), "remote.txt")
	if err := c.Push(context.Background(), "home-pc", local, remote); err != nil {
		t.Fatalf("push: %v", err)
	}
	back := filepath.Join(t.TempDir(), "back.txt")
	if err := c.Pull(context.Background(), "home-pc", remote, back); err != nil {
		t.Fatalf("pull: %v", err)
	}
	got, _ := os.ReadFile(back)
	if string(got) != "http-payload-456" {
		t.Fatalf("round trip mismatch: %q", got)
	}
	_ = strings.TrimSpace
}
