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
	// A device will not raise a pairing request on behalf of a controller that
	// does not say who it is, so reaching the reject-with-URL path needs a label.
	t.Setenv("WANCTL_LABEL", "test controller")
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	trustServer(t, c, "home-pc")

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
	// A device will not raise a pairing request on behalf of a controller that
	// does not say who it is, so reaching the reject-with-URL path needs a label.
	t.Setenv("WANCTL_LABEL", "test controller")
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	trustServer(t, c, "home-pc")

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

// TestPairWithoutLabelIsRefusedOutright covers the other half: an anonymous
// controller must not be able to put a decision in front of the device owner at
// all. It gets an actionable error instead of a pairing URL, because the owner
// has nothing to go on and the controller does.
func TestPairWithoutLabelIsRefusedOutright(t *testing.T) {
	srv := httptest.NewServer(relay.New(relay.EnvTokenStore("tok:alice")).Handler())
	defer srv.Close()
	base := "ws" + strings.TrimPrefix(srv.URL, "http")

	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
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
	t.Setenv("WANCTL_LABEL", "")
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	trustServer(t, c, "home-pc")

	trusted, pairingURL, err := c.Pair(context.Background(), "home-pc")
	if err == nil {
		t.Fatal("an anonymous controller was allowed to raise a pairing request")
	}
	if trusted || pairingURL != "" {
		t.Fatalf("trusted=%v pairingURL=%q; neither should be produced", trusted, pairingURL)
	}
	if !strings.Contains(err.Error(), "wanctl label") {
		t.Fatalf("error does not tell the controller how to become answerable: %v", err)
	}
}
