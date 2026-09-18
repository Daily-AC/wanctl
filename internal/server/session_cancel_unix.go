//go:build !windows

package server

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// processParents snapshots the process table as a pid -> parent-pid map.
//
// `ps -A -o pid,ppid` is the one spelling that works on every system the agent
// runs on: macOS, Linux (procps) and Android (toybox). Reading /proc would
// avoid the fork but does not exist on macOS, and this runs once per cancel.
func processParents() (map[int]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid,ppid").Output()
	if err != nil {
		return nil, fmt.Errorf("snapshot process table: %w", err)
	}
	parents := parseProcessTable(string(out))
	if len(parents) == 0 {
		return nil, errors.New("snapshot process table: no readable rows")
	}
	return parents, nil
}

// terminateProcess ends one process. SIGKILL rather than SIGTERM: the target is
// whatever the user ran, it has no agreed shutdown protocol, and the caller has
// already decided it must stop now.
func terminateProcess(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil // already gone between the snapshot and the signal
		}
		return err
	}
	return nil
}
