package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// fakeStat resolves only the names given, so a subcommand like "exec" behaves
// the way it does in reality: there is no such file.
func fakeStat(t *testing.T, files map[string]string) func(string) (fs.FileInfo, error) {
	t.Helper()
	dir := t.TempDir()
	real := map[string]string{}
	for name, ident := range files {
		p := filepath.Join(dir, ident)
		if _, err := os.Stat(p); err != nil {
			if err := os.WriteFile(p, []byte(ident), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		real[name] = p
	}
	return func(name string) (fs.FileInfo, error) {
		p, ok := real[name]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return os.Stat(p)
	}
}

// The exact shape measured on the tablet: argv[0] as typed (relative), argv[1]
// the absolute path Termux resolved — the same file under two names.
func TestNormalizeArgsDropsTermuxDuplicatedArgv0(t *testing.T) {
	if runtime.GOOS != "android" {
		t.Skip("normalizeArgs only rewrites on android")
	}
	abs := "/data/data/com.termux/files/home/wanctl"
	stat := fakeStat(t, map[string]string{"./wanctl": "wanctl", abs: "wanctl"})
	got := normalizeArgs([]string{"./wanctl", abs, "agent", "--yes"}, stat)
	want := []string{"./wanctl", "agent", "--yes"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

// Identical strings are the simplest form of the same shape.
func TestNormalizeArgsDropsIdenticalArgv0(t *testing.T) {
	if runtime.GOOS != "android" {
		t.Skip("normalizeArgs only rewrites on android")
	}
	p := "/data/data/com.termux/files/home/wanctl"
	got := normalizeArgs([]string{p, p, "version"}, fakeStat(t, nil))
	if want := []string{p, "version"}; !slices.Equal(got, want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

// A real subcommand never stats, so an ordinary invocation is untouched —
// including the adb-shell shape on Android itself.
func TestNormalizeArgsLeavesRealSubcommandsAlone(t *testing.T) {
	stat := fakeStat(t, map[string]string{"/data/local/tmp/wanctl": "wanctl", "./wanctl": "wanctl"})
	for _, args := range [][]string{
		{"/data/local/tmp/wanctl", "version"},
		{"./wanctl", "exec", "--target", "x"},
		{"wanctl"},
		{},
	} {
		before := append([]string(nil), args...)
		if got := normalizeArgs(args, stat); !slices.Equal(got, before) {
			t.Errorf("normalizeArgs(%q) = %q, want unchanged", before, got)
		}
	}
}

// Pushing the binary itself is a legitimate command whose *first* argument is
// still a subcommand, so it must survive.
func TestNormalizeArgsKeepsPushOfTheBinaryItself(t *testing.T) {
	stat := fakeStat(t, map[string]string{"./wanctl": "wanctl"})
	args := []string{"./wanctl", "push", "./wanctl", "/remote/wanctl"}
	before := append([]string(nil), args...)
	if got := normalizeArgs(args, stat); !slices.Equal(got, before) {
		t.Fatalf("got %q want unchanged %q", got, before)
	}
}

// The rewrite must not mutate the caller's slice: os.Args is shared, and
// clobbering its backing array would corrupt what other readers see.
func TestNormalizeArgsDoesNotMutateItsInput(t *testing.T) {
	p := "/x/wanctl"
	args := []string{p, p, "a", "b"}
	_ = normalizeArgs(args, fakeStat(t, nil))
	if !slices.Equal(args, []string{p, p, "a", "b"}) {
		t.Fatalf("input was mutated: %q", args)
	}
}
