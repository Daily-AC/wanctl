package main

import (
	"encoding/base64"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"wanctl/internal/config"
	"wanctl/internal/transport"
)

// TestBuilds ensures the whole module compiles and vets cleanly.
func TestBuilds(t *testing.T) {
	if out, err := exec.Command("go", "build", "./...").CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	if out, err := exec.Command("go", "vet", "./...").CombinedOutput(); err != nil {
		t.Fatalf("vet failed: %v\n%s", err, out)
	}
}

func TestCmdTrustServerPinsVerifiedIdentity(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_TOKEN", "tok")
	t.Setenv("WANCTL_RELAY", "http://relay.invalid")
	fp := transport.Fingerprint([]byte("verified device cert"))
	if err := cmdTrust([]string{"server", "--target", "alice/build", "--fingerprint", fp}); err != nil {
		t.Fatal(err)
	}
	store, err := transport.OpenStore("known_servers.json")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := store.GetByName("alice/build")
	if !ok || got.Fingerprint != fp {
		t.Fatalf("CLI pin missing: %+v, ok=%v", got, ok)
	}
}

func TestPortalAdminsCLIUpdatesAdminAndTrustStores(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	oldFP, newFP := mainTestFP(1), mainTestFP(2)
	if err := cmdPortalAdmins([]string{"add", "--fingerprints", oldFP + "," + newFP}); err != nil {
		t.Fatal(err)
	}
	admins, err := config.OpenPortalAdmins()
	if err != nil {
		t.Fatal(err)
	}
	known, err := transport.OpenStore("known_clients.json")
	if err != nil {
		t.Fatal(err)
	}
	if !admins.Contains(oldFP) || !admins.Contains(newFP) || !known.Has(oldFP) || !known.Has(newFP) {
		t.Fatalf("seed did not update both stores: admins=%v old=%v new=%v", admins.List(), known.Has(oldFP), known.Has(newFP))
	}
	if err := cmdPortalAdmins([]string{"remove", oldFP}); err != nil {
		t.Fatal(err)
	}
	if admins.Contains(oldFP) || known.Has(oldFP) || !admins.Contains(newFP) || !known.Has(newFP) {
		t.Fatalf("rotation did not revoke only old root: admins=%v old=%v new=%v", admins.List(), known.Has(oldFP), known.Has(newFP))
	}
	if err := cmdPortalAdmins([]string{"remove", newFP}); !errors.Is(err, config.ErrLastPortalAdmin) {
		t.Fatalf("remove final portal admin error = %v", err)
	}
}

func mainTestFP(value byte) string {
	return "SHA256:" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string([]byte{value}), 32)))
}
