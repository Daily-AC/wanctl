package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeExe puts an executable file in dir. The bit matters: LookPath will not
// consider a PATH entry without it, and neither would a shell.
func writeExe(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestPathShadow pins the question the update actually has to answer: after the
// swap, does typing `wanctl` run the binary we just replaced? The predecessor
// of this code answered a different question — "is there another file named
// wanctl anywhere on PATH" — and deleted whatever it found.
func TestPathShadow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("resolution order and the executable bit differ on Windows; this fixture would not model it")
	}

	t.Run("the updated copy is what PATH finds", func(t *testing.T) {
		dir := t.TempDir()
		self := writeExe(t, dir, "wanctl", 0o755)
		t.Setenv("PATH", dir)
		if got := pathShadow(self); got != "" {
			t.Errorf("pathShadow = %q, want no shadow", got)
		}
	})

	t.Run("an earlier PATH entry shadows it", func(t *testing.T) {
		selfDir, otherDir := t.TempDir(), t.TempDir()
		self := writeExe(t, selfDir, "wanctl", 0o755)
		other := writeExe(t, otherDir, "wanctl", 0o755)
		t.Setenv("PATH", strings.Join([]string{otherDir, selfDir}, string(os.PathListSeparator)))
		if got := pathShadow(self); got != other {
			t.Errorf("pathShadow = %q, want %q", got, other)
		}
	})

	t.Run("a later PATH entry does not", func(t *testing.T) {
		selfDir, otherDir := t.TempDir(), t.TempDir()
		self := writeExe(t, selfDir, "wanctl", 0o755)
		writeExe(t, otherDir, "wanctl", 0o755)
		t.Setenv("PATH", strings.Join([]string{selfDir, otherDir}, string(os.PathListSeparator)))
		if got := pathShadow(self); got != "" {
			t.Errorf("pathShadow = %q, want no shadow: that copy is inert", got)
		}
	})

	// The common install shape — /usr/local/bin/wanctl symlinked into
	// ~/.local/bin — must not read as two rival copies.
	t.Run("a symlink to the updated copy is not a shadow", func(t *testing.T) {
		selfDir, linkDir := t.TempDir(), t.TempDir()
		self := writeExe(t, selfDir, "wanctl", 0o755)
		if err := os.Symlink(self, filepath.Join(linkDir, "wanctl")); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", strings.Join([]string{linkDir, selfDir}, string(os.PathListSeparator)))
		if got := pathShadow(self); got != "" {
			t.Errorf("pathShadow = %q, want no shadow", got)
		}
	})

	// `sudo /opt/wanctl/bin/wanctl update` where PATH only carries an older
	// copy: the update succeeded and still changes nothing for a bare `wanctl`.
	t.Run("the updated copy is not on PATH at all", func(t *testing.T) {
		selfDir, otherDir := t.TempDir(), t.TempDir()
		self := writeExe(t, selfDir, "wanctl", 0o755)
		other := writeExe(t, otherDir, "wanctl", 0o755)
		t.Setenv("PATH", otherDir)
		if got := pathShadow(self); got != other {
			t.Errorf("pathShadow = %q, want %q", got, other)
		}
	})

	t.Run("a non-executable file of the same name is not a shadow", func(t *testing.T) {
		selfDir, otherDir := t.TempDir(), t.TempDir()
		self := writeExe(t, selfDir, "wanctl", 0o755)
		writeExe(t, otherDir, "wanctl", 0o644)
		t.Setenv("PATH", strings.Join([]string{otherDir, selfDir}, string(os.PathListSeparator)))
		if got := pathShadow(self); got != "" {
			t.Errorf("pathShadow = %q, want no shadow: a shell would not run it either", got)
		}
	})

	t.Run("nothing named wanctl on PATH", func(t *testing.T) {
		selfDir, emptyDir := t.TempDir(), t.TempDir()
		self := writeExe(t, selfDir, "wanctl", 0o755)
		t.Setenv("PATH", emptyDir)
		if got := pathShadow(self); got != "" {
			t.Errorf("pathShadow = %q, want no shadow", got)
		}
	})
}

// TestPathShadowLeavesEveryCopyInPlace is the regression this change exists
// for. The old routine deleted every other file named wanctl on PATH — as root
// under `sudo wanctl update`, where nothing stopped it — without ever
// establishing that any of them was a wanctl binary.
func TestPathShadowLeavesEveryCopyInPlace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("resolution order and the executable bit differ on Windows; this fixture would not model it")
	}
	selfDir, wrapperDir, packagedDir := t.TempDir(), t.TempDir(), t.TempDir()
	self := writeExe(t, selfDir, "wanctl", 0o755)
	wrapper := writeExe(t, wrapperDir, "wanctl", 0o755) // e.g. a script that sets WANCTL_RELAY
	packaged := writeExe(t, packagedDir, "wanctl", 0o755)
	t.Setenv("PATH", strings.Join([]string{wrapperDir, selfDir, packagedDir}, string(os.PathListSeparator)))

	reportPATHShadow(self)

	for _, path := range []string{self, wrapper, packaged} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s no longer exists: %v", path, err)
		}
	}
}
