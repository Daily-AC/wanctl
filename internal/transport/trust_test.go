package transport

import (
	"errors"
	"testing"
)

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

func TestServerPinsAreNamespacedAndReplaceIsExplicit(t *testing.T) {
	store := NewMemStore()
	aliceFP := Fingerprint([]byte("alice cert"))
	bobFP := Fingerprint([]byte("bob cert"))
	if err := store.Pin("alice/build", aliceFP, false); err != nil {
		t.Fatal(err)
	}
	if err := store.Pin("bob/build", bobFP, false); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.GetByName("alice/build"); got.Fingerprint != aliceFP {
		t.Fatalf("alice pin = %q, want %q", got.Fingerprint, aliceFP)
	}
	if got, _ := store.GetByName("bob/build"); got.Fingerprint != bobFP {
		t.Fatalf("bob pin = %q, want %q", got.Fingerprint, bobFP)
	}

	replacement := Fingerprint([]byte("replacement cert"))
	var mismatch *MismatchError
	if err := store.Pin("alice/build", replacement, false); !errors.As(err, &mismatch) {
		t.Fatalf("replace without confirmation: want MismatchError, got %v", err)
	}
	if got, _ := store.GetByName("alice/build"); got.Fingerprint != aliceFP {
		t.Fatalf("failed replacement changed pin to %q", got.Fingerprint)
	}
	if err := store.Pin("alice/build", replacement, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.GetByName("alice/build"); got.Fingerprint != replacement {
		t.Fatalf("explicit replacement left pin at %q", got.Fingerprint)
	}
}
