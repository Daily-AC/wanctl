//go:build !windows

package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func waitForPIDFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil && strings.TrimSpace(string(b)) != "" {
			pid, convErr := strconv.Atoi(strings.TrimSpace(string(b)))
			if convErr != nil {
				t.Fatalf("pid file %q: %v", b, convErr)
			}
			time.Sleep(200 * time.Millisecond) // let it settle under the shell
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the command never wrote its pid to %s", path)
	return 0
}

func processGone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return errors.Is(err, syscall.ESRCH)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return syscall.Kill(pid, 0) != nil
}

// The whole point of option (a): a cancelled command dies, the shell it ran in
// does not, and the working directory an earlier command set is still there
// afterwards.
func TestExecInDirContextKillsTheCommandAndKeepsTheSession(t *testing.T) {
	session, err := NewShellSession("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if code, err := session.Exec("cd "+dir, &out); err != nil || code != 0 {
		t.Fatalf("cd = %d %v", code, err)
	}

	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		code int
		err  error
		took time.Duration
	}
	done := make(chan result, 1)
	go func() {
		var discard strings.Builder
		start := time.Now()
		code, err := session.ExecInDirContext(ctx, "sleep 600 & echo $! > "+pidFile+"; wait", "", &discard)
		done <- result{code: code, err: err, took: time.Since(start)}
	}()

	pid := waitForPIDFile(t, pidFile)
	t.Cleanup(func() { syscall.Kill(pid, syscall.SIGKILL) })
	cancel()

	select {
	case r := <-done:
		if r.err == nil {
			t.Fatalf("cancelled command reported success (exit %d); the caller cannot tell it was stopped", r.code)
		}
		if !errors.Is(r.err, context.Canceled) {
			t.Fatalf("cancelled command reported %v, want context.Canceled", r.err)
		}
		if r.took > 5*time.Second {
			t.Fatalf("the command took %s to come back after the cancel", r.took)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ExecInDirContext never returned after the cancel")
	}

	if !processGone(pid, 3*time.Second) {
		t.Fatalf("process %d survived the cancel", pid)
	}
	if session.Closed() {
		t.Fatal("the session shell was torn down; option (a) keeps it")
	}

	// The acceptance from the issue: the next command works and still sees the
	// directory the session was left in.
	out.Reset()
	code, err := session.Exec("pwd", &out)
	if err != nil || code != 0 {
		t.Fatalf("command after cancel = %d %v", code, err)
	}
	if got := strings.TrimSpace(out.String()); got != dir {
		t.Fatalf("session cwd after cancel = %q, want %q", got, dir)
	}
}

// A command that finished on its own must not be reported as cancelled just
// because the context was torn down afterwards, and nothing may be killed.
func TestExecInDirContextLeavesAFinishedCommandAlone(t *testing.T) {
	session, err := NewShellSession("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	killed := 0
	session.killDescendants = func(int) error { killed++; return nil }

	ctx, cancel := context.WithCancel(context.Background())
	var out strings.Builder
	code, err := session.ExecInDirContext(ctx, "echo still-here", "", &out)
	cancel()
	if err != nil || code != 0 || !strings.Contains(out.String(), "still-here") {
		t.Fatalf("exec = %d %v out=%q", code, err, out.String())
	}
	if killed != 0 {
		t.Fatalf("the cancel hook fired %d times for a command that finished on its own", killed)
	}
}
