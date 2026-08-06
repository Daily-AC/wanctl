package main

import (
	"io/fs"
	"os"
	"runtime"
)

// normalizeArgs undoes Termux's argv[0] duplication.
//
// Go's Android target is always dynamic PIE — `-buildmode=exe` does not even
// compile there — so every wanctl binary asks for `/system/bin/linker64` as its
// ELF interpreter. Termux's exec interceptor (`libtermux-exec.so`, preloaded
// into its shell) sees an interpreter that is not Termux's own and re-executes
// the program through the linker explicitly, which prepends the program path to
// the argument vector. Measured on a vivo PA2353, same binary, 2026-08-06:
//
//	adb shell:  ./probe version → argv = [./probe, version]
//	Termux:     ./probe version → argv = [./probe, /abs/path/probe, version]
//
// Every argument is then shifted by one, so `wanctl version` is read as a
// subcommand named after the binary and wanctl is unusable in the one
// environment Android users actually type commands in. None of this is visible
// over adb, which is why it survived the first round of Android work.
//
// Two cheaper-looking markers were tried on the device and both failed:
//
//   - argv[1] == argv[0]: Termux inserts the *resolved absolute* path while
//     argv[0] keeps whatever was typed, so `./wanctl` never matches.
//   - argv[1] == os.Executable(): under the linker the process's own exe is
//     `/apex/com.android.runtime/bin/linker64`, not wanctl at all.
//
// What does hold is that argv[0] and argv[1] name the same file on disk, which
// os.SameFile answers correctly through relative paths and symlinks alike. A
// real subcommand ("exec", "push", "version") never stats successfully, so the
// rewrite cannot swallow a valid argument.
func normalizeArgs(args []string, stat func(string) (fs.FileInfo, error)) []string {
	if runtime.GOOS != "android" || len(args) < 2 {
		return args
	}
	if !sameFile(args[0], args[1], stat) {
		return args
	}
	// Copy rather than slice-and-append in place: args aliases os.Args, and
	// reusing its backing array would rewrite the vector other readers see.
	out := make([]string, 0, len(args)-1)
	out = append(out, args[0])
	return append(out, args[2:]...)
}

func sameFile(a, b string, stat func(string) (fs.FileInfo, error)) bool {
	if a == b {
		return true
	}
	ai, err := stat(a)
	if err != nil {
		return false
	}
	bi, err := stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

func init() {
	os.Args = normalizeArgs(os.Args, os.Stat)
}
