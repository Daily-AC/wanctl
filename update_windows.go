//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// replaceBinary swaps wanctl.exe on Windows. The running .exe can't be opened
// for write or replaced directly, but it can be renamed, so the current file is
// moved aside to a backup name and the new binary moved into its place.
//
// The backup name is <dst>.old unless that file cannot be removed, which means
// it is itself a running image: the `__supervise` loop `wanctl service install`
// starts runs as long as the user stays logged on, from the file the first
// update moved to .old. Windows refuses to replace a running image, so every
// later update failed with "Access is denied" until that supervisor restarted
// (S18, 2026-10-02). The backup then gets a timestamped name beside it instead.
func replaceBinary(src, dst string) error {
	removeReplacedBinaries(dst)
	old := dst + ".old"
	if _, err := os.Lstat(old); err == nil {
		old = dst + ".old-" + time.Now().Format("20060102-150405")
	}
	if err := os.Rename(dst, old); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", dst, old, err)
	}
	if err := os.Rename(src, dst); err != nil {
		// Try to roll back so the user is never left without a binary at dst.
		_ = os.Rename(old, dst)
		return fmt.Errorf("rename %s -> %s: %w", src, dst, err)
	}
	return nil
}

// removeReplacedBinaries deletes the backups earlier updates left beside dst.
// One that is still running cannot be deleted and stays until a later call:
// each agent start and each update makes one.
func removeReplacedBinaries(dst string) {
	dir, base := filepath.Split(dst)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if name := e.Name(); name == base+".old" || strings.HasPrefix(name, base+".old-") {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}
