//go:build !windows

package main

import "os"

// replaceBinary atomically swaps the running binary on Unix. The currently
// executing inode stays alive (the running process keeps its file descriptor)
// while the path now points at the new binary, ready for the next exec.
func replaceBinary(src, dst string) error {
	return os.Rename(src, dst)
}
