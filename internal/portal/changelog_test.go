package portal

import (
	"strings"
	"testing"
)

func TestReleasesNewestFirst(t *testing.T) {
	rs := releases()
	if len(rs) == 0 {
		t.Fatal("no embedded release notes")
	}
	if rs[0].Version != CurrentVersion() {
		t.Fatalf("head = %q, CurrentVersion = %q", rs[0].Version, CurrentVersion())
	}
	for i := 1; i < len(rs); i++ {
		if compareVersions(rs[i-1].Version, rs[i].Version) <= 0 {
			t.Fatalf("not ordered: %s before %s", rs[i-1].Version, rs[i].Version)
		}
	}
	for _, r := range rs {
		if r.Body == "" {
			t.Fatalf("%s has an empty body", r.Version)
		}
	}
}

// The file name is what CurrentVersion reports and what the portal's badge
// shows; the heading is what the reader sees inside the sheet. A file copied
// from the previous release and renamed but not retitled would leave those two
// disagreeing, and neither of them would be obviously wrong on its own.
//
// This cannot check that an entry *exists* for a release: the release version
// is only injected at link time (-ldflags -X main.buildVersion), so the test
// binary sees "dev". That check lives in scripts/validate-release.sh, which
// both the manual publisher and .github/workflows/release.yml run.
func TestChangelogHeadingMatchesVersion(t *testing.T) {
	for _, r := range releases() {
		head, _, _ := strings.Cut(r.Body, "\n")
		if !strings.HasPrefix(head, "# "+r.Version+" ") && head != "# "+r.Version {
			t.Errorf("%s.md: heading %q does not name %s", r.Version, head, r.Version)
		}
	}
}

// String ordering would put v0.1.10 before v0.1.9.
func TestCompareVersionsIsNumeric(t *testing.T) {
	if compareVersions("v0.1.10", "v0.1.9") <= 0 {
		t.Fatal("v0.1.10 must sort above v0.1.9")
	}
	if compareVersions("v0.2.0", "v0.1.99") <= 0 {
		t.Fatal("v0.2.0 must sort above v0.1.99")
	}
	if compareVersions("v0.1.5", "v0.1.5") != 0 {
		t.Fatal("equal versions must compare equal")
	}
}
