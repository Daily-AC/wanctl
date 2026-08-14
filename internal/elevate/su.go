package elevate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// suSearchPath is where a su binary lives, in the order the common root
// managers install it. `su` is deliberately not looked up on PATH: on Android
// PATH is whatever the parent process happened to have (under Termux it leads
// into an app-private directory the agent cannot exec at all — ADR 0003), and
// resolving a privilege-granting binary through a mutable search path is the
// wrong shape for this even when it works.
var suSearchPath = []string{
	"/system/bin/su",
	"/system/xbin/su",
	"/sbin/su",
	"/su/bin/su",
	"/debug_ramdisk/su", // Magisk on Android 12+
	"/magisk/.core/bin/su",
}

// SuProbeTimeout bounds the consent dialog. A root manager raises one on the
// first request from a new app and the device may be in someone's pocket, so
// this must end rather than wedge the command that triggered it.
const SuProbeTimeout = 20 * time.Second

// Su is the root channel: it runs commands through a su binary. This is the
// only channel that keeps working across a reboot with nobody touching the
// device, which is why it is probed first.
type Su struct {
	// searchPath is suSearchPath except in tests, which point it at a stub so
	// the probe and the run path are exercised for real rather than mocked.
	searchPath []string
	stat       func(string) (os.FileInfo, error)
	shell      string
	path       string

	mu   sync.Mutex
	form suForm // learned by Probe, reused by Run
}

// suForm is how this device's su wants to be told to run a command. There is no
// single answer: `su -c CMD` is what Magisk, KernelSU and APatch accept, and
// AOSP's own su — the one on userdebug builds and emulator images — rejects it
// outright ("invalid uid/gid '-c'") because its grammar is `su [WHO [ARGS...]]`.
// Measured on an android-29 google_apis emulator image, 2026-08-14:
//
//	su -c id            → su: invalid uid/gid '-c'
//	su root sh -c id    → uid=0(root) …
//
// Rather than guess from the device or the root manager's name, the probe tries
// the forms and keeps the one that answered as root.
type suForm int

const (
	suFormUnknown suForm = iota
	suFormDashC          // su -c CMD          (Magisk, KernelSU, APatch)
	suFormUserSh         // su root sh -c CMD  (AOSP)
)

// NewSu builds the root channel.
func NewSu() *Su {
	return &Su{searchPath: suSearchPath, stat: os.Stat, shell: "/system/bin/sh"}
}

func (s *Su) Kind() Kind { return KindSu }

// find returns the first su binary present, or "".
func (s *Su) find() string {
	if s.path != "" {
		return s.path
	}
	stat := s.stat
	if stat == nil {
		stat = os.Stat
	}
	search := s.searchPath
	if search == nil {
		search = suSearchPath
	}
	for _, p := range search {
		if fi, err := stat(p); err == nil && !fi.IsDir() {
			s.path = p
			return p
		}
	}
	return ""
}

// Probe runs `id` through su and keeps the invocation form that answered as
// root. Presence of the binary is not enough: a root manager can be installed
// and still deny this app, and on a device where nobody taps the consent dialog
// the honest answer is "no", not "maybe".
func (s *Su) Probe(ctx context.Context) Status {
	path := s.find()
	if path == "" {
		return Status{Available: false, Reason: "no su binary found; the device does not appear to be rooted"}
	}
	ctx, cancel := context.WithTimeout(ctx, SuProbeTimeout)
	defer cancel()

	var last string
	for _, form := range []suForm{suFormDashC, suFormUserSh} {
		var buf bytes.Buffer
		code, err := s.runForm(ctx, form, "id", "", &buf)
		out := firstLine(strings.TrimSpace(buf.String()))
		switch {
		case ctx.Err() == context.DeadlineExceeded:
			return Status{Available: false, Reason: fmt.Sprintf(
				"su did not answer within %s; the root manager is probably waiting for "+
					"someone to grant wanctl access on the device", SuProbeTimeout)}
		case err != nil:
			last = fmt.Sprintf("%s failed: %v", path, err)
			continue
		case code != 0:
			if out == "" {
				out = fmt.Sprintf("exit %d", code)
			}
			last = "su refused: " + out
			continue
		case !strings.Contains(out, "uid=0"):
			// A su that exits 0 without being root is not a channel, it is a
			// trap: every command after it would run unprivileged in silence.
			last = "su succeeded but did not yield root: " + out
			continue
		}
		s.mu.Lock()
		s.form = form
		s.mu.Unlock()
		return Status{Available: true, Detail: out}
	}
	if last == "" {
		last = "su did not yield root in any known invocation form"
	}
	return Status{Available: false, Reason: last}
}

// Run executes command as root, using the invocation form Probe settled on.
//
// In every form the command occupies exactly one argv slot and su hands it to a
// shell — the same contract `sh -c` has — so nothing here re-splits it. The
// quoting hazard this project documents for `exec` (v0.1.12) does not get a
// second chance to appear at this layer. cwd travels as a process attribute
// rather than as a `cd` prefix for the same reason.
func (s *Su) Run(ctx context.Context, command, cwd string, out io.Writer) (int, error) {
	s.mu.Lock()
	form := s.form
	s.mu.Unlock()
	if form == suFormUnknown {
		// Run before Probe (or after a probe that never succeeded). Default to
		// the form the overwhelming majority of rooted devices use; a device
		// that needs the other one will have been probed first in practice,
		// because Manager.Select always probes before it runs anything.
		form = suFormDashC
	}
	return s.runForm(ctx, form, command, cwd, out)
}

func (s *Su) runForm(ctx context.Context, form suForm, command, cwd string, out io.Writer) (int, error) {
	path := s.find()
	if path == "" {
		return -1, fmt.Errorf("no su binary found")
	}
	shell := s.shell
	if shell == "" {
		shell = "/system/bin/sh"
	}
	var argv []string
	switch form {
	case suFormUserSh:
		argv = []string{"root", shell, "-c", command}
	default:
		argv = []string{"-c", command}
	}
	cmd := exec.CommandContext(ctx, path, argv...)
	cmd.Dir = cwd
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return -1, ctxErr
	}
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return -1, err
}

func (s *Su) Close() error { return nil }

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
