package config

import "testing"

func TestProductionDefaults(t *testing.T) {
	if DefaultRelay != "https://wanctl-relay.***REMOVED***.***REMOVED***.com" {
		t.Fatalf("DefaultRelay = %q", DefaultRelay)
	}
	if DefaultTransport != "http" {
		t.Fatalf("DefaultTransport = %q", DefaultTransport)
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
