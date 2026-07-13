//go:build !windows

package config

import (
	"errors"
	"os"
	"syscall"
)

func lockAgentFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return errAgentLockHeld
	}
	return err
}
