//go:build !darwin && !linux

package server

// os.Root still prevents symlink escape on supported platforms. Unix adds
// O_NOFOLLOW and O_NONBLOCK to reject final symlinks and avoid blocking on FIFOs.
const secureOpenFlags = 0
