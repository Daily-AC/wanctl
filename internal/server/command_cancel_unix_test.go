//go:build !windows

package server

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Cancelling a one-shot must kill what the command started, not just the shell.
// The agent runs `sh -c`, the interesting process is a child of that shell, and
// only the process-group kill in configureCommandCancellation reaches it.
func TestRunOneShotContextKillsTheProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		_, err := RunOneShotContext(ctx, "/bin/sh", "sleep 60 & echo $! > "+pidFile+"; wait", "", &out)
		done <- err
	}()

	pid := 0
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if b, err := os.ReadFile(pidFile); err == nil && strings.TrimSpace(string(b)) != "" {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("command never reported its child pid")
	}
	defer syscall.Kill(pid, syscall.SIGKILL)
	time.Sleep(200 * time.Millisecond)

	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled command returned no error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunOneShotContext did not return after cancellation")
	}

	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child %d survived the cancelled one-shot", pid)
}
