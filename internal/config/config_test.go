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
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	old := DefaultRelay
	DefaultRelay = ""
	t.Cleanup(func() { DefaultRelay = old })
	t.Setenv("WANCTL_RELAY", "")

	if _, err := Relay(); err == nil || !strings.Contains(err.Error(), "wanctl config set relay=") {
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
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	old := DefaultPortal
	DefaultPortal = ""
	t.Cleanup(func() { DefaultPortal = old })
	t.Setenv("WANCTL_PORTAL", "")

	if _, err := Portal(); err == nil || !strings.Contains(err.Error(), "wanctl config set portal=") {
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

// The persisted config sits between the environment and the build default:
// env overrides it, and it overrides what the binary was built with.
func TestSettingPrecedence(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", "")
	old := DefaultRelay
	DefaultRelay = "https://built.example"
	t.Cleanup(func() { DefaultRelay = old })

	if v, src := Setting("relay"); v != "https://built.example" || src != "build default" {
		t.Fatalf("build-default layer: %q from %q", v, src)
	}
	if err := SaveSetting("relay", "https://file.example"); err != nil {
		t.Fatal(err)
	}
	if v, src := Setting("relay"); v != "https://file.example" || src != "config file" {
		t.Fatalf("file layer: %q from %q", v, src)
	}
	t.Setenv("WANCTL_RELAY", "https://env.example")
	if v, src := Setting("relay"); v != "https://env.example" || !strings.HasPrefix(src, "env ") {
		t.Fatalf("env layer: %q from %q", v, src)
	}
	t.Setenv("WANCTL_RELAY", "")
	if err := RemoveSetting("relay"); err != nil {
		t.Fatal(err)
	}
	if v, _ := Setting("relay"); v != "https://built.example" {
		t.Fatalf("after unset: %q", v)
	}
}

func TestTransportDefaultsToHTTP(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_TRANSPORT", "")
	if got := Transport(); got != "http" {
		t.Fatalf("Transport() = %q", got)
	}
	if err := SaveSetting("transport", "ws"); err != nil {
		t.Fatal(err)
	}
	if got := Transport(); got != "ws" {
		t.Fatalf("persisted Transport() = %q", got)
	}
}

func TestSettingRejectsUnknownKeys(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	if err := SaveSetting("nonsense", "x"); err == nil {
		t.Fatal("SaveSetting accepted an unknown key")
	}
	if KnownSetting("nonsense") {
		t.Fatal("KnownSetting(nonsense) = true")
	}
	if v, src := Setting("nonsense"); v != "" || src != "" {
		t.Fatalf("Setting(nonsense) = %q, %q", v, src)
	}
}
