package transport

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// LoadOrCreateDeviceID identifies an installation independently of its name and
// certificate. Publish with a hard link so concurrent starts never return two IDs.
func LoadOrCreateDeviceID() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "device_id")
	read := func() (string, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		id := strings.TrimSpace(string(data))
		if !ValidDeviceID(id) {
			return "", fmt.Errorf("invalid device ID in %s; restore the original file", path)
		}
		return id, nil
	}
	if id, err := read(); !os.IsNotExist(err) {
		return id, err
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".device-id-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if _, err = f.WriteString(id.String() + "\n"); err != nil {
		f.Close()
		return "", err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return "", err
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	if err = os.Link(f.Name(), path); err != nil && !os.IsExist(err) {
		return "", err
	}
	return read()
}

func ValidDeviceID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.Version() == 4 && parsed.Variant() == uuid.RFC4122 && parsed.String() == id
}
