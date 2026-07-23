package main

import (
	"os/exec"
	"testing"

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
