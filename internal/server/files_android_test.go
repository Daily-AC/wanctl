package server

import (
	"path/filepath"
	"testing"
)

// On Android the walk from "/" to a writable directory crosses traverse-only
// directories (/data is drwxrwx--x system:system), and os.Root needs to *open*
// each one. Rooting at the target's own directory is what makes a transfer
// reach /data/local/tmp at all.
func TestUnconstrainedRootIsTheTargetDirOnAndroid(t *testing.T) {
	got := unconstrainedRootFor("android", "/data/local/tmp/payload.bin")
	if want := "/data/local/tmp"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// Every other platform keeps the volume root, so nothing about existing
// deployments changes.
func TestUnconstrainedRootKeepsVolumeRootElsewhere(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		if got := unconstrainedRootFor(goos, "/tmp/payload.bin"); got != "/" {
			t.Fatalf("%s: got %q want %q", goos, got, "/")
		}
	}
}

// The narrower root must not change which paths are accepted: the file still
// resolves, and it still resolves to the same place.
func TestRootedNameResolvesTheSameFileUnderTheAndroidRoot(t *testing.T) {
	target := filepath.Join(t.TempDir(), "payload.bin")
	root, name, err := rootedName(filepath.Dir(target), target)
	if err != nil {
		t.Fatalf("rootedName: %v", err)
	}
	if joined := filepath.Join(root, name); joined != target {
		t.Fatalf("resolved to %q, want %q", joined, target)
	}
	if name != "payload.bin" {
		t.Fatalf("name = %q, want payload.bin", name)
	}
}

// Escaping the root stays refused — the narrower root makes this stricter, not
// looser.
func TestRootedNameStillRefusesEscape(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := rootedName(filepath.Join(dir, "inner"), filepath.Join(dir, "outside.bin")); err == nil {
		t.Fatal("expected an escape refusal")
	}
}
