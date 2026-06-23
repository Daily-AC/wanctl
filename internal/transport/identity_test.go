package transport

import (
	"os"
	"strings"
	"testing"
)

func TestLoadOrCreateIdentityIsStable(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())

	id1, err := LoadOrCreateIdentity()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(id1.Fingerprint, "SHA256:") {
		t.Fatalf("bad fingerprint %q", id1.Fingerprint)
	}
	id2, err := LoadOrCreateIdentity()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if id1.Fingerprint != id2.Fingerprint {
		t.Fatalf("identity not stable: %s vs %s", id1.Fingerprint, id2.Fingerprint)
	}
}

func TestFingerprintMatchesCert(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	id, err := LoadOrCreateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if Fingerprint(id.Cert.Certificate[0]) != id.Fingerprint {
		t.Fatal("Fingerprint(cert) != id.Fingerprint")
	}
	_ = os.Stdout
}
