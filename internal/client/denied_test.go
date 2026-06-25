package client

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wanctl/internal/agent"
	"wanctl/internal/relay"
)

// A command with no matching rule, against a headless agent in normal mode
// (deny-by-default), must return a clean error — not hang the controller.
func TestExecDeniedReturnsError(t *testing.T) {
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	ag, err := agent.New(agent.Options{RelayURL: base, Token: "tok", Name: "home-pc", AutoYes: true}) // normal mode, headless -> deny
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ag.Run(ctx)
	time.Sleep(200 * time.Millisecond)

	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", base)
	t.Setenv("WANCTL_TOKEN", "tok")
	t.Setenv("WANCTL_TRANSPORT", "ws") // this test wires a ws relay; New() now defaults to http
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	var code int
	var execErr error
	go func() {
		code, execErr = c.Exec(context.Background(), "home-pc", "whoami", true, "")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("denied exec hung instead of returning an error")
	}
	if execErr == nil || code != -1 || !strings.Contains(execErr.Error(), "denied by device policy") {
		t.Fatalf("expected policy-denied error, got code=%d err=%v", code, execErr)
	}
}
