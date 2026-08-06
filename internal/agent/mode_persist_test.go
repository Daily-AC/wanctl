package agent

import (
	"os"
	"path/filepath"
	"testing"

	"wanctl/internal/policy"
)

// A device switched to bypass — by `--mode bypass` once, or from the portal —
// must still be in bypass after a flag-less restart. That is exactly what a
// service unit does: `service install` deliberately omits --mode so the
// persisted mode survives, and policy.Open documents an empty mode as "use the
// persisted one". Filling in a default before calling it makes that
// unreachable, so the portal's switch is silently undone by the next restart.
func TestAgentWithoutModeUsesThePersistedMode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WANCTL_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "mode"), []byte("bypass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ag, err := New(Options{RelayURL: "ws://x", Token: "t", Name: "dev1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := ag.engine.Mode(); got != policy.ModeBypass {
		t.Fatalf("mode = %q, want %q — the persisted mode was overridden by a default", got, policy.ModeBypass)
	}
}

// An explicit flag still wins over what is on disk, and is remembered.
func TestExplicitModeStillOverridesThePersistedOne(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WANCTL_CONFIG_DIR", dir)
	if err := os.WriteFile(filepath.Join(dir, "mode"), []byte("bypass\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ag, err := New(Options{RelayURL: "ws://x", Token: "t", Name: "dev1", Mode: policy.ModeNormal})
	if err != nil {
		t.Fatal(err)
	}
	if got := ag.engine.Mode(); got != policy.ModeNormal {
		t.Fatalf("mode = %q, want %q", got, policy.ModeNormal)
	}
}

// With nothing persisted and no flag, a device is in normal mode — the safe
// default must not become "whatever was left over".
func TestNoFlagAndNothingPersistedIsNormal(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	ag, err := New(Options{RelayURL: "ws://x", Token: "t", Name: "dev1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := ag.engine.Mode(); got != policy.ModeNormal {
		t.Fatalf("mode = %q, want %q", got, policy.ModeNormal)
	}
}
