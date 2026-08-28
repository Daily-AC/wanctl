package server

import (
	"os"
	"path/filepath"
	"testing"
)

// A write grant (or bypass) whose policy root reaches the config dir must not
// be able to overwrite the device's own trust/identity/policy files, nor read
// the identity key out (audit 2026-08-28, SEC-D1-01). rootedName backs both the
// upload and download paths, so guarding it covers read and write.
func TestRootedNameRefusesConfigDirAndBinary(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("WANCTL_CONFIG_DIR", cfg)
	// Create the files an escalation would target.
	for _, name := range []string{"portal_admins.json", "rules.json", "key.pem", "token"} {
		if err := os.WriteFile(filepath.Join(cfg, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A global/bypass decision passes policyRoot="" (whole volume).
	for _, name := range []string{"portal_admins.json", "rules.json", "key.pem", "token"} {
		if _, _, err := rootedName("", filepath.Join(cfg, name)); err == nil {
			t.Errorf("%s was transferable through an empty policy root", name)
		}
		// A not-yet-existing file inside the config dir is refused too.
	}
	if _, _, err := rootedName("", filepath.Join(cfg, "new-evil.json")); err == nil {
		t.Error("a new file inside the config dir was allowed")
	}
	// The running test binary stands in for the wanctl binary.
	if self, err := os.Executable(); err == nil {
		if _, _, err := rootedName("", self); err == nil {
			t.Error("the running binary was transferable")
		}
	}
	// A normal file outside the config dir is unaffected.
	outside := filepath.Join(t.TempDir(), "data.txt")
	os.WriteFile(outside, []byte("x"), 0o644)
	if _, _, err := rootedName("", outside); err != nil {
		t.Errorf("an ordinary file was refused: %v", err)
	}
}
