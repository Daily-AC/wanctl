package portal

import "testing"

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
