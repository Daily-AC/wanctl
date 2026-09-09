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

// AgentRunning reports whether an agent is serving this config dir, and the pid
// it recorded.
//
// Liveness comes from the lock, which a running agent holds for its whole life,
// and never from the number in agent.pid. That number is a label, not an
// identity: a pid file outlives the agent that wrote it when the process is
// killed without `wanctl stop`, and the operating system hands the same number
// to something unrelated soon enough. A controller-only Windows machine
// registered itself as a controlled device exactly that way -- `wanctl update`
// read a stale agent.pid, found that pid alive, and "restarted" an agent that
// had never been running.
//
// The probe is a non-blocking try-acquire, so it cannot disturb the agent that
// holds the lock. A pid of 0 with running true means an agent holds the lock
// but recorded no pid: callers may report it, but have nothing to signal.
func AgentRunning() (pid int, running bool) {
	lock, err := AcquireAgentLock()
	if err == nil {
		lock.Close()
		return ReadPID(), false
	}
	if IsAgentLockHeld(err) {
		return ReadPID(), true
	}
	// The lock could not be evaluated at all (unreadable config dir, a
	// filesystem without locking). Report not running: the agent's own
	// AcquireAgentLock is what actually keeps two agents apart, so being wrong
	// here costs a redundant start or a refused stop, never a second agent.
	return ReadPID(), false
}

// Close releases the lock.
func (l *AgentLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	return l.file.Close()
}
