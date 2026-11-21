package release

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestArtifactArch(t *testing.T) {
	for _, tc := range []struct {
		goos, goarch string
		major        uint32
		want         string
	}{
		{"windows", "amd64", 6, Windows7Arch}, // 7 is 6.1, 8 is 6.2, 8.1 is 6.3
		{"windows", "amd64", 0, Windows7Arch}, // an unreadable version errs towards the build that starts everywhere
		{"windows", "amd64", 10, "amd64"},     // 10 and 11 both report 10
		{"windows", "amd64", 11, "amd64"},
		// No 32-bit or ARM build starts below Windows 10, so there is nothing to
		// switch to; the installer refuses those machines instead.
		{"windows", "386", 6, "386"},
		{"windows", "arm64", 6, "arm64"},
		{"windows", "386", 10, "386"},
		{"linux", "amd64", 0, "amd64"},
		{"linux", "amd64", 6, "amd64"},
		{"linux", "amd64", 10, "amd64"},
		{"darwin", "arm64", 6, "arm64"},
		{"android", "arm64", 6, "arm64"},
	} {
		if got := ArtifactArch(tc.goos, tc.goarch, tc.major); got != tc.want {
			t.Errorf("ArtifactArch(%s, %s, %d) = %q, want %q", tc.goos, tc.goarch, tc.major, got, tc.want)
		}
	}
	if runtime.GOOS != "windows" && HostArtifactArch() != runtime.GOARCH {
		t.Errorf("HostArtifactArch() = %q on %s/%s", HostArtifactArch(), runtime.GOOS, runtime.GOARCH)
	}
}

func windowsManifest(t *testing.T, arches ...string) Manifest {
	t.Helper()
	m := Manifest{Schema: 1, Version: "v2.0.0", PublishedAt: time.Now().UTC().Truncate(time.Second)}
	for _, arch := range arches {
		m.Artifacts = append(m.Artifacts, Artifact{
			OS: "windows", Arch: arch, Name: ArtifactName("windows", arch), Size: 1,
			SHA256: strings.Repeat("0", 64),
		})
	}
	// Round-trip through the parser every client runs, so the fixture is a
	// manifest a client would actually accept.
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

// A Windows 7 machine must never be handed the plain windows/amd64 build: it
// would install and then never start again, and with it the updater that could
// repair it. A release from before the win7 build existed has no entry for it,
// and that is a failed check, not a reason to take the other one.
func TestSelectNeverSubstitutesThePlainWindowsBuild(t *testing.T) {
	old := windowsManifest(t, "amd64", "386", "arm64")
	if _, err := Select(old, "windows", Windows7Arch, "v1.0.0"); err == nil ||
		!strings.Contains(err.Error(), "no artifact for windows/amd64.win7") {
		t.Fatalf("Select on a release without the win7 build: %v", err)
	}
	if _, err := SelectExact(old, "windows", Windows7Arch, "v2.0.0"); err == nil ||
		!strings.Contains(err.Error(), "no artifact for windows/amd64.win7") {
		t.Fatalf("SelectExact on a release without the win7 build: %v", err)
	}

	both := windowsManifest(t, "amd64", Windows7Arch, "386")
	for name, pick := range map[string]func() (Artifact, error){
		"Select":      func() (Artifact, error) { return Select(both, "windows", Windows7Arch, "v1.0.0") },
		"SelectExact": func() (Artifact, error) { return SelectExact(both, "windows", Windows7Arch, "v2.0.0") },
	} {
		got, err := pick()
		if err != nil || got.Arch != Windows7Arch || got.Name != "wanctl-windows-amd64.win7.exe" {
			t.Fatalf("%s for windows/amd64.win7 = %+v, %v", name, got, err)
		}
	}
	// And the other way round: Windows 10 keeps the official build.
	if got, err := Select(both, "windows", "amd64", "v1.0.0"); err != nil || got.Name != "wanctl-windows-amd64.exe" {
		t.Fatalf("Select for windows/amd64 = %+v, %v", got, err)
	}
}

// The name is bound to the platform by the signed manifest itself, so a
// manifest cannot list the plain binary under the win7 arch either.
func TestManifestBindsTheWin7NameToItsArch(t *testing.T) {
	m := Manifest{Schema: 1, Version: "v2.0.0", PublishedAt: time.Now().UTC(), Artifacts: []Artifact{{
		OS: "windows", Arch: Windows7Arch, Name: "wanctl-windows-amd64.exe", Size: 1, SHA256: strings.Repeat("0", 64),
	}}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseManifest(raw); err == nil || !strings.Contains(err.Error(), "does not match windows/amd64.win7") {
		t.Fatalf("ParseManifest accepted the plain build under the win7 arch: %v", err)
	}
}
