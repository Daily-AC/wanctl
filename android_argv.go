package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// normalizeArgs undoes Termux's argv[0] duplication.
//
// Go's Android target is always dynamic PIE — `-buildmode=exe` does not even
// compile there — so every wanctl binary asks for `/system/bin/linker64` as its
// ELF interpreter. Termux's exec interceptor (`libtermux-exec.so`, preloaded
// into its shell) sees an interpreter that is not Termux's own and re-executes
// the program through the linker explicitly, which prepends the program's
// resolved absolute path to the argument vector. Measured on a vivo PA2353:
//
//	adb shell:  ./probe version → argv = [./probe, version]
//	Termux:     ./probe version → argv = [./probe, /abs/path/probe, version]
//
// Every argument is then shifted by one, so `wanctl version` is read as a
// subcommand named after the binary and wanctl is unusable in the one
// environment Android users actually type commands in. None of it is visible
// over adb, where no interception happens.
//
// The marker is that argv[1] is an absolute path to a regular file whose base
// name matches argv[0]'s. That covers all three ways argv[0] reaches us —
// found on PATH ("wanctl"), relative ("./wanctl"), or absolute — which matters
// because the bare form is what everyone types after installing, and it is the
// one that broke v0.1.7 in Termux.
//
// Three narrower markers were tried on the device and each failed:
//
//   - argv[1] == argv[0]: Termux inserts the resolved absolute path while
//     argv[0] keeps what was typed, so "./wanctl" never matches.
//   - argv[1] == os.Executable(): under the linker the process's own exe is
//     "/apex/com.android.runtime/bin/linker64", not wanctl.
//   - os.SameFile(stat(argv[0]), stat(argv[1])): a bare "wanctl" resolved from
//     PATH does not stat relative to the working directory.
//
// A real subcommand ("exec", "push", "version") is never an absolute path, so
// the rewrite cannot swallow a valid argument.
func normalizeArgs(args []string, stat func(string) (fs.FileInfo, error)) []string {
	if runtime.GOOS != "android" || len(args) < 2 {
		return args
	}
	if !duplicatedArgv0(args[0], args[1], stat) {
		return args
	}
	// Copy rather than slice-and-append in place: args aliases os.Args, and
	// reusing its backing array would rewrite the vector other readers see.
	out := make([]string, 0, len(args)-1)
	out = append(out, args[0])
	return append(out, args[2:]...)
}

func duplicatedArgv0(argv0, argv1 string, stat func(string) (fs.FileInfo, error)) bool {
	if argv0 == argv1 && argv0 != "" {
		return true
	}
	// Termux always inserts an absolute path; a subcommand never is one.
	if !filepath.IsAbs(argv1) || filepath.Base(argv0) != filepath.Base(argv1) {
		return false
	}
	fi, err := stat(argv1)
	if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
		return false
	}
	return true
}

func init() {
	os.Args = normalizeArgs(os.Args, os.Stat)
}
