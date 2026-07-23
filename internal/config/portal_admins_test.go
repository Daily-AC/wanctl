package config

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestSelfHostedBuildHasNoPortalFingerprint(t *testing.T) {
	if DefaultPortalFP != "" {
		t.Fatalf("compiled binary carries a portal root: %q", DefaultPortalFP)
	}
}

func TestParsePortalFingerprintsStrictAndDeduplicated(t *testing.T) {
	a, b := configTestFP(1), configTestFP(2)
	got, err := ParsePortalFingerprints(a + ", " + b + "," + a)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("parsed fingerprints = %v", got)
	}
	for _, invalid := range []string{"sha256:bad", "SHA256:bad", a + ",," + b} {
		if _, err := ParsePortalFingerprints(invalid); err == nil {
			t.Fatalf("invalid fingerprint list accepted: %q", invalid)
		}
	}
}

func TestPortalAdminsOverlapAndLastAdminProtection(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	a, b := configTestFP(1), configTestFP(2)
	admins, err := OpenPortalAdmins()
	if err != nil {
		t.Fatal(err)
	}
	if err := admins.Add(a, b, a); err != nil {
		t.Fatal(err)
	}
	if err := admins.Remove(a); err != nil {
		t.Fatal(err)
	}
	if admins.Contains(a) || !admins.Contains(b) {
		t.Fatalf("unexpected set after rotation: %v", admins.List())
	}
	if err := admins.Remove(b); !errors.Is(err, ErrLastPortalAdmin) {
		t.Fatalf("remove last admin error = %v", err)
	}
	reopened, err := OpenPortalAdmins()
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.List(); len(got) != 1 || got[0] != b {
		t.Fatalf("persisted admins = %v", got)
	}
}

func configTestFP(value byte) string {
	return "SHA256:" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string([]byte{value}), 32)))
}
