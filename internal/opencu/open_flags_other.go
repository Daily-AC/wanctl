//go:build !darwin && !linux

package opencu

// The Lstat and SameFile checks in openRegular still refuse symlinks and
// non-regular files; unix adds O_NOFOLLOW and O_NONBLOCK to the open itself.
const cacheOpenFlags = 0
