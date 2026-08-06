package main

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	// The path Termux inserted is the one thing here that reliably names this
	// binary, so keep it: os.Executable() cannot, and several commands need it.
	// See selfPath.
	termuxSelf = args[1]
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

// termuxSelf is the program's real path, recorded when Termux's exec
// interception hands it to us as a duplicated argv[0]. Empty everywhere else.
var termuxSelf string

// selfPath returns the path of the running wanctl binary.
//
// os.Executable() reads /proc/self/exe, which is normally exactly right — but
// under Termux the process really is the dynamic linker, so it answers
// "/apex/com.android.runtime/bin/linker64". Everything that locates wanctl to
// act on it then aims at the wrong file, and the consequences are not
// symmetric: `wanctl update` atomically replaces that path, so on a rooted
// device it would overwrite the system linker and take the Android runtime with
// it. (Unrooted, it fails with a permission error naming linker64 — which is
// how this was found.) `wanctl start` would likewise exec the linker with
// "agent" as its argument.
//
// When Termux told us the real path via the duplicated argv[0], that is the
// authoritative answer. Otherwise — every other OS, and an adb shell on Android
// where nothing intercepts exec — os.Executable() is correct.
func selfPath() (string, error) {
	if termuxSelf != "" {
		return termuxSelf, nil
	}
	return os.Executable()
}

// selfCommand builds a command that runs this wanctl binary again.
//
// Normally that is just exec.Command(self, ...). Under Termux it cannot be:
// Android refuses exec of a file inside an app's private data directory from
// the untrusted_app domain, so `wanctl start` failed with
//
//	fork/exec /data/data/com.termux/files/usr/bin/wanctl: permission denied
//
// even though that binary is what is currently running. Termux's own
// interceptor gets around this by invoking the dynamic linker explicitly and
// passing the program as its first argument, and nothing stops us doing the
// same — the linker lives in /apex or /system, which the domain may exec.
//
// The argument vector is built to look exactly like what Termux produces
// (program path twice, then the real arguments) so the child's own
// normalizeArgs strips the duplicate and parses its flags correctly.
func selfCommand(self string, args ...string) *exec.Cmd {
	if termuxSelf == "" {
		return exec.Command(self, args...)
	}
	linker := androidLinker()
	if linker == "" {
		return exec.Command(self, args...) // no linker found; fail the honest way
	}
	cmd := exec.Command(linker)
	cmd.Args = append([]string{self, self}, args...)
	return cmd
}

// androidLinker locates the dynamic linker used to start this process. Under
// Termux os.Executable() already names it, which is the most accurate answer
// available; the fixed paths are a fallback for a layout that reports something
// else.
func androidLinker() string {
	if exe, err := os.Executable(); err == nil && strings.Contains(filepath.Base(exe), "linker") {
		return exe
	}
	for _, p := range []string{"/system/bin/linker64", "/apex/com.android.runtime/bin/linker64"} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}
