package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"wanctl/internal/transport"
)

// fileIn returns <configdir>/name, creating the config dir if needed.
func fileIn(name string) (string, error) {
	dir, err := transport.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// TokenPath is where the OAuth-enrolled agent token is stored.
func TokenPath() (string, error) { return fileIn("token") }

// StoredToken returns the token saved by `wanctl up`, or "" if none. Errors are
// swallowed (treated as "no token") so it composes cleanly as an EnvOr default.
func StoredToken() string {
	p, err := TokenPath()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// SaveToken writes the token to the config dir with owner-only permissions.
func SaveToken(token string) error {
	p, err := TokenPath()
	if err != nil {
		return err
	}
	return os.WriteFile(p, []byte(strings.TrimSpace(token)+"\n"), 0o600)
}

// ClearToken removes the stored token (logout). Missing file is not an error.
func ClearToken() error {
	p, err := TokenPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// PIDPath / LogPath locate the background agent's pid and log files.
func PIDPath() (string, error) { return fileIn("agent.pid") }
func LogPath() (string, error) { return fileIn("agent.log") }

// ReadPID returns the recorded background-agent pid, or 0 if none recorded.
func ReadPID() int {
	p, err := PIDPath()
	if err != nil {
		return 0
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

// WritePID records the background-agent pid.
func WritePID(pid int) error {
	p, err := PIDPath()
	if err != nil {
		return err
	}
	return os.WriteFile(p, []byte(strconv.Itoa(pid)+"\n"), 0o600)
}

// RemovePID clears the recorded pid (on stop). Missing file is not an error.
func RemovePID() error {
	p, err := PIDPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
