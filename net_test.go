package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"wanctl/internal/config"
)

func TestCmdNetLanRequiresConfiguration(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_LAN_RELAY", "")
	old := config.DefaultLanRelay
	config.DefaultLanRelay = ""
	t.Cleanup(func() { config.DefaultLanRelay = old })

	err := cmdNet([]string{"lan"})
	if err == nil || !strings.Contains(err.Error(), "no LAN relay configured (set WANCTL_LAN_RELAY)") {
		t.Fatalf("cmdNet(lan) error = %v", err)
	}
	if got := config.StoredNetMode(); got != "wan" {
		t.Fatalf("network mode changed to %q after rejected lan configuration", got)
	}
}

func TestCmdNetStatusShowsUnconfiguredRelays(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", "")
	t.Setenv("WANCTL_LAN_RELAY", "")
	oldRelay, oldLanRelay := config.DefaultRelay, config.DefaultLanRelay
	config.DefaultRelay, config.DefaultLanRelay = "", ""
	t.Cleanup(func() {
		config.DefaultRelay, config.DefaultLanRelay = oldRelay, oldLanRelay
	})

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdout := os.Stdout
	os.Stdout = w
	err = cmdNet([]string{"status"})
	w.Close()
	os.Stdout = oldStdout
	out, readErr := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got := strings.Count(string(out), "(not configured)"); got != 2 {
		t.Fatalf("status output has %d unconfigured markers, want 2:\n%s", got, out)
	}
}
