package transport

import "testing"

func TestStoreReloadsOnMissAfterAnotherStoreAddsPeer(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())

	store1, err := OpenStore("known_clients.json")
	if err != nil {
		t.Fatalf("open store1: %v", err)
	}
	store2, err := OpenStore("known_clients.json")
	if err != nil {
		t.Fatalf("open store2: %v", err)
	}

	const fp = "SHA256:from-another-process"
	if store1.Has(fp) {
		t.Fatal("store1 unexpectedly had fingerprint before add")
	}
	if err := store2.Add(fp, "controller"); err != nil {
		t.Fatalf("store2 add: %v", err)
	}
	if !store1.Has(fp) {
		t.Fatal("store1 did not reload missing fingerprint added by store2")
	}
}
