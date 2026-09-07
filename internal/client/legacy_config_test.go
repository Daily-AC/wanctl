package client

import (
	"os"
	"path/filepath"
	"testing"

	"wanctl/internal/config"
)

// TestLegacyLanConfigIsIgnored pins the upgrade path from a build that still
// had the LAN fast path: the config directory keeps a `netmode` file reading
// "lan" and the device-side `lan` switch, and WANCTL_LAN_RELAY may still be
// exported by an old service unit. None of that may fail startup or divert the
// controller — it resolves the ordinary relay setting and nothing else.
func TestLegacyLanConfigIsIgnored(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WANCTL_CONFIG_DIR", dir)
	t.Setenv("WANCTL_RELAY", "")
	t.Setenv("WANCTL_TRANSPORT", "")
	t.Setenv("WANCTL_LAN_RELAY", "ws://192.0.2.1:8080")
	t.Setenv("WANCTL_TOKEN", "tok")

	for name, body := range map[string]string{
		"netmode": "lan\n",
		"lan":     "off\n",
		"relay":   "https://relay.example\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// `wanctl config` reads the same layer and must not choke on the strays.
	if got, _ := config.Setting("relay"); got != "https://relay.example" {
		t.Fatalf("relay setting = %q", got)
	}

	c, err := New()
	if err != nil {
		t.Fatalf("New() with legacy lan config: %v", err)
	}
	if c.RelayURL() != "https://relay.example" {
		t.Fatalf("relay = %q, want the persisted public relay", c.RelayURL())
	}
}
