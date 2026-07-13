package config

import "testing"

func TestAcquireAgentLockExcludesSameConfigDirAndReleases(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())

	first, err := AcquireAgentLock()
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	second, err := AcquireAgentLock()
	if err == nil {
		second.Close()
		t.Fatal("second lock unexpectedly succeeded")
	}
	if !IsAgentLockHeld(err) {
		t.Fatalf("second lock error = %v, want held", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first lock: %v", err)
	}
	third, err := AcquireAgentLock()
	if err != nil {
		t.Fatalf("third lock after release: %v", err)
	}
	third.Close()
}
