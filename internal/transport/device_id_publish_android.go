package transport

import (
	"errors"
	"os"
	"syscall"
)

// Android's untrusted-app policy prohibits hard links in app data. Serialize
// creators with a kernel lock, then rename the fully synced file atomically.
// The lock file stays in place: unlinking it would let waiters lock different
// inodes. Process exit releases flock, so a killed creator cannot strand it.
func publishDeviceID(temp, path string) error {
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	// Never replace a winner, an invalid ID, or a symlink. The caller reads
	// and validates the published file after an ErrExist result.
	if _, err := os.Lstat(path); err == nil {
		return os.ErrExist
	} else if !os.IsNotExist(err) {
		return err
	}
	return os.Rename(temp, path)
}
