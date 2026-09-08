package transport

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestDeviceIDPersistsAcrossConcurrentStartsAndCertificateReplacement(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WANCTL_CONFIG_DIR", dir)
	ids := make(chan string, 32)
	errs := make(chan error, 32)
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() { id, err := LoadOrCreateDeviceID(); ids <- id; errs <- err })
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	first := ""
	for id := range ids {
		if !ValidDeviceID(id) {
			t.Fatal(id)
		}
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("concurrent starts returned %s and %s", first, id)
		}
	}
	cert, err := LoadOrCreateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "cert.pem")); err != nil {
		t.Fatal(err)
	}
	replacement, err := LoadOrCreateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if cert.Fingerprint == replacement.Fingerprint {
		t.Fatal("test did not rotate the certificate")
	}
	if id, err := LoadOrCreateDeviceID(); err != nil || id != first {
		t.Fatalf("certificate rotation changed ID: %s %v", id, err)
	}
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	if id, err := LoadOrCreateDeviceID(); err != nil || id == first {
		t.Fatalf("fresh installation reused ID: %s %v", id, err)
	}
}

func TestInvalidDeviceIDIsNotSilentlyReplaced(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WANCTL_CONFIG_DIR", dir)
	p := filepath.Join(dir, "device_id")
	if err := os.WriteFile(p, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateDeviceID(); err == nil || !strings.Contains(err.Error(), "initialize device ID:") {
		t.Fatal("corruption must fail with the app fatal marker rather than orphan existing permissions")
	}
	b, _ := os.ReadFile(p)
	if string(b) != "broken" {
		t.Fatal("corrupt ID was overwritten")
	}
}
