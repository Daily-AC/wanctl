package opencu

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
)

const maxVersionManifestBytes = 16 << 10

// InstalledVersion is metadata-only discovery. The agent calls it only from
// its single background reader: even Stat/Open on a redirected filesystem may
// block in the OS and cannot be made cancellable by a Go context.
func InstalledVersion() string { return readInstalledVersion(versionManifestPath()) }

func readInstalledVersion(path string) string {
	if path == "" {
		return ""
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxVersionManifestBytes {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxVersionManifestBytes {
		return ""
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxVersionManifestBytes+1))
	if err != nil || len(raw) > maxVersionManifestBytes {
		return ""
	}
	var m Manifest
	if json.Unmarshal(raw, &m) != nil || !filepath.IsAbs(m.MCP.Command) {
		return ""
	}
	for _, arg := range m.MCP.Args {
		if !filepath.IsAbs(arg) {
			return ""
		}
	}
	var version string
	if json.Unmarshal(m.Version, &version) != nil {
		return ""
	}
	return version
}

// Pure path policy; Windows supplies token/session/profile lookups. SYSTEM
// session 0 never falls back to its own LOCALAPPDATA, including on API errors.
func selectVersionManifestPath(systemSessionZero bool, local string, session uint32, profile func(uint32) (string, error)) string {
	if systemSessionZero {
		if session == 0 || session == ^uint32(0) {
			return ""
		}
		dir, err := profile(session)
		if err != nil || dir == "" {
			return ""
		}
		local = filepath.Join(dir, "AppData", "Local")
	}
	if local == "" {
		return ""
	}
	return filepath.Join(local, "OpenCU", "opencu.json")
}
