package main

import (
	"bufio"
	"strings"
	"testing"

	"wanctl/internal/config"
)

func TestConfigSetRoundTrip(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	if err := configSet([]string{"relay=https://r.example", "portal=https://p.example", "transport=ws"}); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"relay": "https://r.example", "portal": "https://p.example", "transport": "ws"} {
		if got := config.StoredSetting(k); got != want {
			t.Fatalf("%s = %q, want %q", k, got, want)
		}
	}
	if err := configUnset([]string{"transport"}); err != nil {
		t.Fatal(err)
	}
	if got := config.StoredSetting("transport"); got != "" {
		t.Fatalf("transport after unset = %q", got)
	}
}

// A bad pair anywhere in the arguments must leave the config untouched —
// half-applied settings point commands at mismatched instances.
func TestConfigSetValidatesBeforeWriting(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	if err := configSet([]string{"relay=https://ok.example", "transport=carrier-pigeon"}); err == nil {
		t.Fatal("bogus transport accepted")
	}
	if got := config.StoredSetting("relay"); got != "" {
		t.Fatalf("relay written despite invalid sibling: %q", got)
	}
}

func TestValidateSetting(t *testing.T) {
	for _, ok := range [][2]string{
		{"relay", "https://r.example"}, {"relay", "wss://r.example"},
		{"portal", "http://p.example:8081"}, {"transport", "http"},
	} {
		if err := validateSetting(ok[0], ok[1]); err != nil {
			t.Fatalf("%s=%s rejected: %v", ok[0], ok[1], err)
		}
	}
	for _, bad := range [][2]string{
		{"relay", "not a url"}, {"relay", "ftp://r.example"},
		{"portal", "wss://p.example"}, {"transport", "tcp"}, {"nonsense", "x"},
	} {
		if err := validateSetting(bad[0], bad[1]); err == nil {
			t.Fatalf("%s=%s accepted", bad[0], bad[1])
		}
	}
}

// Under `go test` stdin is not a terminal, so with nothing configured the
// first-run helper must fail with instructions instead of hanging on a prompt.
func TestEnsureEndpointsNonInteractive(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", "")
	t.Setenv("WANCTL_PORTAL", "")
	err := ensureEndpointsConfigured()
	if err == nil || !strings.Contains(err.Error(), "wanctl config set") {
		t.Fatalf("err = %v, want the config-set instruction", err)
	}
	t.Setenv("WANCTL_RELAY", "https://r.example")
	t.Setenv("WANCTL_PORTAL", "https://p.example")
	if err := ensureEndpointsConfigured(); err != nil {
		t.Fatalf("configured endpoints still rejected: %v", err)
	}
}

func TestPromptSettingRetriesThenAccepts(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("not a url\nhttps://r.example\n"))
	v, err := promptSetting(in, "relay", "> ")
	if err != nil || v != "https://r.example" {
		t.Fatalf("got %q, %v", v, err)
	}
	in = bufio.NewReader(strings.NewReader("\n"))
	if _, err := promptSetting(in, "relay", "> "); err == nil {
		t.Fatal("empty input accepted, want cancellation")
	}
}

func TestUpdateSourcePrefersReleaseBase(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELEASE_BASE", "https://github.com/o/r/releases/latest/download/")
	base, err := updateSource()
	if err != nil || base != "https://github.com/o/r/releases/latest/download" {
		t.Fatalf("base = %q, %v", base, err)
	}
	t.Setenv("WANCTL_RELEASE_BASE", "")
	t.Setenv("WANCTL_RELAY", "https://relay.example/")
	base, err = updateSource()
	if err != nil || base != "https://relay.example/dl" {
		t.Fatalf("relay fallback = %q, %v", base, err)
	}
	t.Setenv("WANCTL_RELAY", "")
	if _, err := updateSource(); err == nil {
		t.Fatal("no source configured but updateSource succeeded")
	}
}
