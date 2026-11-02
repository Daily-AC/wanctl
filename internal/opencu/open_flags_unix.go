//go:build darwin || linux

package opencu

import "golang.org/x/sys/unix"

const cacheOpenFlags = unix.O_NOFOLLOW | unix.O_NONBLOCK
