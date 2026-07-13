package config

import (
	"errors"
	"os"
)

var errAgentLockHeld = errors.New("wanctl agent is already running for this config dir")

// AgentLock holds the per-config-dir agent process lock. Closing the file
// releases the OS lock.
type AgentLock struct {
	file *os.File
}

// AcquireAgentLock takes the non-blocking exclusive lock for this config dir.
func AcquireAgentLock() (*AgentLock, error) {
	path, err := fileIn("agent.lock")
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockAgentFile(f); err != nil {
		f.Close()
		return nil, err
	}
	return &AgentLock{file: f}, nil
}

// IsAgentLockHeld reports whether err means another agent already owns the
// config-dir lock.
func IsAgentLockHeld(err error) bool {
	return errors.Is(err, errAgentLockHeld)
}

// Close releases the lock.
func (l *AgentLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	return l.file.Close()
}
