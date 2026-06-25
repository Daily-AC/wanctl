//go:build windows

package main

import (
	"fmt"
	"os"
)

// replaceBinary swaps wanctl.exe on Windows. The running .exe can't be opened
// for write or replaced directly, so we first rename it to <dst>.old (which IS
// allowed) and then move the new binary into place. The .old file lingers
// until the next reboot, then can be deleted manually.
func replaceBinary(src, dst string) error {
	old := dst + ".old"
	_ = os.Remove(old) // best-effort cleanup of a previous run
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
