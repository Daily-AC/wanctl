package config

import (
	"fmt"
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

// LabelPath is where the controller's self-description is persisted. A device
// asked to pair an unknown controller shows this to its owner, so it is the
// difference between "trust SHA256:… from bogon?" and a question a human can
// actually answer.
func LabelPath() (string, error) { return fileIn("label") }

// StoredLabel returns the persisted controller label, or "" if none.
func StoredLabel() string {
	p, err := LabelPath()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// SaveLabel persists the controller label. An empty value clears it.
func SaveLabel(label string) error {
	p, err := LabelPath()
	if err != nil {
		return err
	}
	if strings.TrimSpace(label) == "" {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return os.WriteFile(p, []byte(strings.TrimSpace(label)+"\n"), 0o600)
}

// settingFiles maps `wanctl config` keys to their file in the config dir —
// same one-file-per-value layout as token/mode/label.
var settingFiles = map[string]string{
	"relay":        "relay",
	"portal":       "portal",
	"transport":    "transport",
	"release_base": "release-base",
}

// KnownSetting reports whether key is a persistable endpoint setting.
func KnownSetting(key string) bool { _, ok := settingFiles[key]; return ok }

// StoredSetting returns the persisted value for a setting, or "" if none.
// Errors read as "not set" so it composes as a resolution layer.
func StoredSetting(key string) string {
	name, ok := settingFiles[key]
	if !ok {
		return ""
	}
	p, err := fileIn(name)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// SaveSetting persists a setting with owner-only permissions.
func SaveSetting(key, value string) error {
	name, ok := settingFiles[key]
	if !ok {
		return fmt.Errorf("unknown setting %q", key)
	}
	p, err := fileIn(name)
	if err != nil {
		return err
	}
	return os.WriteFile(p, []byte(strings.TrimSpace(value)+"\n"), 0o600)
}

// RemoveSetting deletes a persisted setting. Missing file is not an error.
func RemoveSetting(key string) error {
	name, ok := settingFiles[key]
	if !ok {
		return fmt.Errorf("unknown setting %q", key)
	}
	p, err := fileIn(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// SettingsDir returns the directory settings persist in, for display.
func SettingsDir() string {
	dir, err := transport.ConfigDir()
	if err != nil {
		return ""
	}
	return dir
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

// ManagedPIDPath records that the current agent is owned by an external
// supervisor. The pid in the file prevents a stale marker from changing the
// lifecycle of a later, independently started agent.
func ManagedPIDPath() (string, error) { return fileIn("agent.managed") }

func ManagedPID() int {
	p, err := ManagedPIDPath()
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

func WriteManagedPID(pid int) error {
	p, err := ManagedPIDPath()
	if err != nil {
		return err
	}
	return os.WriteFile(p, []byte(strconv.Itoa(pid)+"\n"), 0o600)
}

// RemoveManagedPID only removes the marker owned by pid. This avoids an old
// agent deleting the marker of a supervisor replacement that has already won
// the startup race.
func RemoveManagedPID(pid int) error {
	if ManagedPID() != pid {
		return nil
	}
	p, err := ManagedPIDPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
