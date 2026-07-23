//go:build darwin || linux

package server

import "golang.org/x/sys/unix"

const secureOpenFlags = unix.O_NOFOLLOW | unix.O_NONBLOCK
