package client

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"wanctl/internal/config"
)

// TestNetModeResolution verifies how New() picks the relay: persisted mode,
// auto probing, and the WANCTL_RELAY override.
func TestNetModeResolution(t *testing.T) {
	oldDefaultRelay := config.DefaultRelay
	config.DefaultRelay = "https://relay.example"
	t.Cleanup(func() { config.DefaultRelay = oldDefaultRelay })
	dir := t.TempDir()
	t.Setenv("WANCTL_CONFIG_DIR", dir)
	t.Setenv("WANCTL_TOKEN", "tok")
	os.Unsetenv("WANCTL_RELAY")
	os.Unsetenv("WANCTL_TRANSPORT")

	// A fake intranet relay that answers /healthz.
	lanSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Write([]byte("ok"))
			return
		}
		http.NotFound(w, r)
	}))
	defer lanSrv.Close()
	lanWS := strings.Replace(lanSrv.URL, "http:", "ws:", 1)
	t.Setenv("WANCTL_LAN_RELAY", lanWS)

	// Default (no netmode file): wan.
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if c.Lan() || c.RelayURL() != config.DefaultRelay {
		t.Fatalf("default should be wan: lan=%v relay=%s", c.Lan(), c.RelayURL())
	}

	// lan: forced intranet relay over ws.
	if err := config.SaveNetMode("lan"); err != nil {
		t.Fatal(err)
	}
	c, err = New()
	if err != nil {
		t.Fatal(err)
	}
	if !c.Lan() || c.RelayURL() != lanWS || c.transport != "ws" {
		t.Fatalf("lan mode wrong: lan=%v relay=%s tr=%s", c.Lan(), c.RelayURL(), c.transport)
	}

	// auto with reachable lan relay: picks lan.
	if err := config.SaveNetMode("auto"); err != nil {
		t.Fatal(err)
	}
	c, err = New()
	if err != nil {
		t.Fatal(err)
	}
	if !c.Lan() {
		t.Fatalf("auto should pick reachable lan relay, got %s", c.RelayURL())
	}

	// auto with unreachable lan relay: falls back to wan.
	lanSrv.Close()
	c, err = New()
	if err != nil {
		t.Fatal(err)
	}
	if c.Lan() || c.RelayURL() != config.DefaultRelay {
		t.Fatalf("auto fallback wrong: lan=%v relay=%s", c.Lan(), c.RelayURL())
	}

	// Explicit WANCTL_RELAY overrides any mode.
	if err := config.SaveNetMode("lan"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WANCTL_RELAY", "wss://explicit.example")
	c, err = New()
	if err != nil {
		t.Fatal(err)
	}
	if c.Lan() || c.RelayURL() != "wss://explicit.example" {
		t.Fatalf("env override wrong: lan=%v relay=%s", c.Lan(), c.RelayURL())
	}
}

// TestLanReachableTimeout ensures the probe respects its timeout budget.
func TestLanReachableTimeout(t *testing.T) {
	t.Setenv("WANCTL_LAN_RELAY", "ws://10.255.255.1:9") // blackhole
	start := time.Now()
	if LanReachable(300 * time.Millisecond) {
		t.Fatal("blackhole relay reported reachable")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("probe exceeded budget: %v", time.Since(start))
	}
}

func TestNetModeWithoutLanConfiguration(t *testing.T) {
	oldDefaultRelay := config.DefaultRelay
	oldDefaultLanRelay := config.DefaultLanRelay
	config.DefaultRelay = "https://relay.example"
	config.DefaultLanRelay = ""
	t.Cleanup(func() {
		config.DefaultRelay = oldDefaultRelay
		config.DefaultLanRelay = oldDefaultLanRelay
	})
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_TOKEN", "tok")
	t.Setenv("WANCTL_RELAY", "")
	t.Setenv("WANCTL_LAN_RELAY", "")

	if err := config.SaveNetMode("auto"); err != nil {
		t.Fatal(err)
	}
	c, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if c.Lan() || c.RelayURL() != "https://relay.example" {
		t.Fatalf("auto without LAN config: lan=%v relay=%q", c.Lan(), c.RelayURL())
	}

	if err := config.SaveNetMode("lan"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(); err == nil || !strings.Contains(err.Error(), "no LAN relay configured") {
		t.Fatalf("lan without configuration error = %v", err)
	}
}
