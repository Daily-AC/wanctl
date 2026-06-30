package policy

import "testing"

func TestModePersistsAcrossOpen(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())

	// Explicit bypass is recorded.
	e, err := Open("rules.json", ModeBypass)
	if err != nil {
		t.Fatal(err)
	}
	if e.Mode() != ModeBypass {
		t.Fatalf("explicit mode = %q, want bypass", e.Mode())
	}

	// A flag-less restart (mode "") keeps the persisted bypass instead of
	// reverting to normal — the bug this fixes.
	e2, err := Open("rules.json", "")
	if err != nil {
		t.Fatal(err)
	}
	if e2.Mode() != ModeBypass {
		t.Fatalf("restart mode = %q, want persisted bypass", e2.Mode())
	}

	// A runtime switch (e.g. portal toggle) persists too.
	e2.SetMode(ModeNormal)
	e3, err := Open("rules.json", "")
	if err != nil {
		t.Fatal(err)
	}
	if e3.Mode() != ModeNormal {
		t.Fatalf("after SetMode(normal) restart = %q, want normal", e3.Mode())
	}
}

func TestModeDefaultsNormalWhenUnset(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	e, err := Open("rules.json", "")
	if err != nil {
		t.Fatal(err)
	}
	if e.Mode() != ModeNormal {
		t.Fatalf("fresh mode = %q, want normal", e.Mode())
	}
}

func TestExplicitModeDoesNotClobberWithoutFlag(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	// Persist bypass.
	if _, err := Open("rules.json", ModeBypass); err != nil {
		t.Fatal(err)
	}
	// A flag-less open must not write normal over it.
	if _, err := Open("rules.json", ""); err != nil {
		t.Fatal(err)
	}
	final, err := Open("rules.json", "")
	if err != nil {
		t.Fatal(err)
	}
	if final.Mode() != ModeBypass {
		t.Fatalf("flag-less opens clobbered persisted mode: got %q", final.Mode())
	}
}
