package opencu

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestVersionManifestPathSelection(t *testing.T) {
	local := filepath.Join(t.TempDir(), "Local")
	profile := filepath.Join(t.TempDir(), "console-user")
	for _, tc := range []struct {
		name    string
		system  bool
		local   string
		session uint32
		profile string
		err     error
		want    string
		calls   int
	}{
		{"interactive", false, local, 0, "", nil, filepath.Join(local, "OpenCU", "opencu.json"), 0},
		{"no local app data", false, "", 0, "", nil, "", 0},
		{"SYSTEM active user", true, local, 7, profile, nil, filepath.Join(profile, "AppData", "Local", "OpenCU", "opencu.json"), 1},
		{"SYSTEM no console", true, local, ^uint32(0), "", nil, "", 0},
		{"SYSTEM session zero", true, local, 0, "", nil, "", 0},
		{"SYSTEM no user token", true, local, 7, "", errors.New("no token"), "", 1},
		{"SYSTEM missing profile", true, local, 7, "", nil, "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			got := selectVersionManifestPath(tc.system, tc.local, tc.session, func(session uint32) (string, error) {
				calls++
				if session != 7 {
					t.Fatal(session)
				}
				return tc.profile, tc.err
			})
			if got != tc.want || calls != tc.calls {
				t.Fatalf("path=%q calls=%d", got, calls)
			}
		})
	}
}

func TestVersionReaderRejectsNonRegularAndOversizedFiles(t *testing.T) {
	if got := readInstalledVersion(""); got != "" {
		t.Fatal(got)
	}
	dir := t.TempDir()
	if got := readInstalledVersion(dir); got != "" {
		t.Fatal("read directory")
	}
	path := filepath.Join(dir, "manifest")
	for _, raw := range []string{"{broken", strings.Repeat("x", maxVersionManifestBytes+1), `{"version":"` + strings.Repeat("a", 70<<10) + `","mcp":{"command":"/no-launch"}}`} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if got := readInstalledVersion(path); got != "" {
			t.Fatal("invalid manifest accepted")
		}
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(dir, "link")
		if err := os.Symlink(path, link); err != nil {
			t.Fatal(err)
		}
		if got := readInstalledVersion(link); got != "" {
			t.Fatal("followed non-regular link")
		}
		t.Setenv("LOCALAPPDATA", dir)
		if err := os.Mkdir(filepath.Join(dir, "OpenCU"), 0700); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(map[string]any{"version": "0.3.99", "mcp": map[string]any{"command": filepath.Join(dir, "not-started")}})
		if err := os.WriteFile(filepath.Join(dir, "OpenCU", "opencu.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		if got := InstalledVersion(); got != "" {
			t.Fatal("non-Windows discovery")
		}
	}
}
