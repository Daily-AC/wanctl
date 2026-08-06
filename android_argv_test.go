package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// realFile makes name resolvable as a regular executable file, so stat behaves
// like it does on a device; anything else reports "not exist", the way a
// subcommand does.
func statOver(t *testing.T, names ...string) func(string) (fs.FileInfo, error) {
	t.Helper()
	dir := t.TempDir()
	real := map[string]string{}
	for _, n := range names {
		p := filepath.Join(dir, filepath.Base(n))
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
		real[n] = p
	}
	return func(name string) (fs.FileInfo, error) {
		p, ok := real[name]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return os.Stat(p)
	}
}

// All three ways argv[0] can reach us in Termux. The bare form is what a user
// types after installing, and it is what shipped broken in v0.1.7.
func TestNormalizeArgsDropsDuplicateForEveryArgv0Form(t *testing.T) {
	if runtime.GOOS != "android" {
		t.Skip("normalizeArgs only rewrites on android")
	}
	abs := "/data/data/com.termux/files/usr/bin/wanctl"
	stat := statOver(t, abs)
	for _, argv0 := range []string{"wanctl", "./wanctl", abs} {
		got := normalizeArgs([]string{argv0, abs, "version"}, stat)
		want := []string{argv0, "version"}
		if !slices.Equal(got, want) {
			t.Errorf("argv0=%q: got %q want %q", argv0, got, want)
		}
	}
}

// An ordinary invocation is untouched — including the adb-shell shape, where
// nothing intercepts exec and argv[1] is a real subcommand.
func TestNormalizeArgsLeavesRealSubcommandsAlone(t *testing.T) {
	stat := statOver(t, "/data/local/tmp/wanctl")
	for _, args := range [][]string{
		{"/data/local/tmp/wanctl", "version"},
		{"./wanctl", "exec", "--target", "x"},
		{"wanctl", "push", "./wanctl", "/remote/wanctl"},
		{"wanctl"},
		{},
	} {
		before := append([]string(nil), args...)
		if got := normalizeArgs(args, stat); !slices.Equal(got, before) {
			t.Errorf("normalizeArgs(%q) = %q, want unchanged", before, got)
		}
	}
}

// A same-named file elsewhere must not trigger the rewrite: only the program's
// own absolute path is inserted, and a caller naming some other binary is
// passing an argument, not a duplicate.
func TestNormalizeArgsIgnoresRelativeOrMismatchedSecondArg(t *testing.T) {
	if runtime.GOOS != "android" {
		t.Skip("normalizeArgs only rewrites on android")
	}
	stat := statOver(t, "/usr/bin/other", "./wanctl")
	for _, args := range [][]string{
		{"wanctl", "./wanctl", "version"},   // relative: Termux always inserts absolute
		{"wanctl", "/usr/bin/other", "run"}, // different base name
	} {
		before := append([]string(nil), args...)
		if got := normalizeArgs(args, stat); !slices.Equal(got, before) {
			t.Errorf("normalizeArgs(%q) = %q, want unchanged", before, got)
		}
	}
}

// The rewrite must not mutate the caller's slice: os.Args is shared.
func TestNormalizeArgsDoesNotMutateItsInput(t *testing.T) {
	p := "/x/wanctl"
	args := []string{p, p, "a", "b"}
	_ = normalizeArgs(args, statOver(t))
	if !slices.Equal(args, []string{p, p, "a", "b"}) {
		t.Fatalf("input was mutated: %q", args)
	}
}

// selfPath must prefer what Termux told us: os.Executable() there answers with
// the dynamic linker, and `wanctl update` replaces whatever it is given — on a
// rooted device that would overwrite /apex/.../linker64 and take the Android
// runtime down with it.
func TestSelfPathPrefersTheTermuxSuppliedPath(t *testing.T) {
	real := "/data/data/com.termux/files/usr/bin/wanctl"
	t.Cleanup(func() { termuxSelf = "" })
	termuxSelf = real
	got, err := selfPath()
	if err != nil || got != real {
		t.Fatalf("selfPath() = %q, %v; want %q", got, err, real)
	}
}

// Everywhere else — including an adb shell, where nothing intercepts exec —
// os.Executable() is correct and must still be used.
func TestSelfPathFallsBackToOsExecutable(t *testing.T) {
	t.Cleanup(func() { termuxSelf = "" })
	termuxSelf = ""
	want, err := os.Executable()
	if err != nil {
		t.Skip("os.Executable unavailable")
	}
	got, err := selfPath()
	if err != nil || got != want {
		t.Fatalf("selfPath() = %q, %v; want %q", got, err, want)
	}
}

// The recorded path must come from the rewrite, so a normal invocation never
// sets it.
func TestNormalizeArgsRecordsSelfOnlyWhenItRewrites(t *testing.T) {
	t.Cleanup(func() { termuxSelf = "" })
	termuxSelf = ""
	normalizeArgs([]string{"wanctl", "exec", "--target", "x"}, statOver(t))
	if termuxSelf != "" {
		t.Fatalf("recorded %q on an ordinary invocation", termuxSelf)
	}
	if runtime.GOOS != "android" {
		return
	}
	abs := "/data/data/com.termux/files/usr/bin/wanctl"
	normalizeArgs([]string{"wanctl", abs, "version"}, statOver(t, abs))
	if termuxSelf != abs {
		t.Fatalf("termuxSelf = %q, want %q", termuxSelf, abs)
	}
}
