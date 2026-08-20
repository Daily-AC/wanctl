package config

import (
	"strings"
	"testing"
)

func TestDeploymentDefaultsAreEmpty(t *testing.T) {
	if DefaultRelay != "" {
		t.Fatalf("DefaultRelay = %q, want empty", DefaultRelay)
	}
	if DefaultPortal != "" {
		t.Fatalf("DefaultPortal = %q, want empty", DefaultPortal)
	}
	if DefaultLanRelay != "" {
		t.Fatalf("DefaultLanRelay = %q, want empty", DefaultLanRelay)
	}
	if DefaultTransport != "http" {
		t.Fatalf("DefaultTransport = %q", DefaultTransport)
	}
}

func TestRelayResolution(t *testing.T) {
	old := DefaultRelay
	DefaultRelay = ""
	t.Cleanup(func() { DefaultRelay = old })
	t.Setenv("WANCTL_RELAY", "")

	if _, err := Relay(); err == nil || !strings.Contains(err.Error(), "set WANCTL_RELAY=https://your-relay") {
		t.Fatalf("Relay() error = %v", err)
	}
	t.Setenv("WANCTL_RELAY", "https://env-relay.example")
	DefaultRelay = "https://built-relay.example"
	got, err := Relay()
	if err != nil || got != "https://env-relay.example" {
		t.Fatalf("Relay() = %q, %v", got, err)
	}
}

func TestPortalResolution(t *testing.T) {
	old := DefaultPortal
	DefaultPortal = ""
	t.Cleanup(func() { DefaultPortal = old })
	t.Setenv("WANCTL_PORTAL", "")

	if _, err := Portal(); err == nil || !strings.Contains(err.Error(), "set WANCTL_PORTAL=https://your-portal") {
		t.Fatalf("Portal() error = %v", err)
	}
	t.Setenv("WANCTL_PORTAL", "https://env-portal.example")
	DefaultPortal = "https://built-portal.example"
	got, err := Portal()
	if err != nil || got != "https://env-portal.example" {
		t.Fatalf("Portal() = %q, %v", got, err)
	}
}

// A controller's label is what makes a pairing request answerable, so it has to
// survive the shell that set it.
func TestLabelRoundTrip(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	if got := StoredLabel(); got != "" {
		t.Fatalf("fresh config dir has label %q", got)
	}
	if err := SaveLabel("  张三的 MacBook / Claude Code  "); err != nil {
		t.Fatal(err)
	}
	if got := StoredLabel(); got != "张三的 MacBook / Claude Code" {
		t.Fatalf("label = %q", got)
	}
	if err := SaveLabel(""); err != nil {
		t.Fatal(err)
	}
	if got := StoredLabel(); got != "" {
		t.Fatalf("cleared label = %q", got)
	}
}
