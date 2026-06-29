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

// TestPairAlreadyTrusted exercises the happy path: a controller talking to an
// agent that auto-trusts (AutoYes) gets trusted=true and an empty pairing URL.
func TestPairAlreadyTrusted(t *testing.T) {
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	ag, err := agent.New(agent.Options{RelayURL: base, Token: "tok", Name: "home-pc", AutoYes: true})
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
	t.Setenv("WANCTL_TRANSPORT", "ws")
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}

	trusted, pairingURL, err := c.Pair(context.Background(), "home-pc")
	if err != nil {
		t.Fatalf("Pair: unexpected err: %v", err)
	}
	if !trusted {
		t.Fatalf("Pair: expected trusted=true (AutoYes), got false; pairingURL=%q", pairingURL)
	}
	if pairingURL != "" {
		t.Errorf("Pair: trusted=true should imply empty pairingURL, got %q", pairingURL)
	}
}

// TestPairRequiresApproval exercises the reject path: a normal-mode agent with
// no portal hooked up returns KindReject + PairingURL. Pair() must NOT surface
// that as an error — it should return trusted=false plus the URL with err=nil
// so the MCP / CLI caller can hand the URL to the user directly.
func TestPairRequiresApproval(t *testing.T) {
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	// AutoYes false + no console subscriber => agent rejects with PairingURL.
	ag, err := agent.New(agent.Options{RelayURL: base, Token: "tok", Name: "home-pc"})
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
	t.Setenv("WANCTL_TRANSPORT", "ws")
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}

	trusted, pairingURL, err := c.Pair(context.Background(), "home-pc")
	if err != nil {
		t.Fatalf("Pair: reject path should NOT surface as error, got: %v", err)
	}
	if trusted {
		t.Errorf("Pair: expected trusted=false on reject, got true")
	}
	if pairingURL == "" {
		t.Fatal("Pair: reject path must return a non-empty pairingURL")
	}
	if !strings.Contains(pairingURL, "/#pair?") || !strings.Contains(pairingURL, "device=home-pc") {
		t.Errorf("pairingURL %q doesn't look like the portal pair URL with device=home-pc", pairingURL)
	}
}
