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
	"wanctl/internal/relay"
)

func TestClientExecAndFileRoundTrip(t *testing.T) {
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	// Agent.
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	ag, err := agent.New(agent.Options{RelayURL: base, Token: "tok", Name: "home-pc", AutoYes: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)
	time.Sleep(200 * time.Millisecond)

	// Controller (separate config dir).
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", base)
	t.Setenv("WANCTL_TOKEN", "tok")
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}

	code, err := c.Exec(context.Background(), "home-pc", "echo hi", true)
	if err != nil || code != 0 {
		t.Fatalf("exec: code=%d err=%v", code, err)
	}

	// Push then pull a file, verify contents survive the round trip.
	local := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(local, []byte("payload-123"), 0o644)
	remote := filepath.Join(t.TempDir(), "remote.txt")
	if err := c.Push(context.Background(), "home-pc", local, remote); err != nil {
		t.Fatalf("push: %v", err)
	}
	back := filepath.Join(t.TempDir(), "back.txt")
	if err := c.Pull(context.Background(), "home-pc", remote, back); err != nil {
		t.Fatalf("pull: %v", err)
	}
	got, _ := os.ReadFile(back)
	if string(got) != "payload-123" {
		t.Fatalf("round trip mismatch: %q", got)
	}
}
