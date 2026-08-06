package server

import "testing"

// The system shell is the only one the agent can exec on Android; Termux's own
// shell is refused by SELinux (see androidShell's comment), so it must never be
// chosen even when it exists.
func TestAndroidShellPrefersTheSystemShell(t *testing.T) {
	present := map[string]bool{
		"/data/data/com.termux/files/usr/bin/sh": true,
		"/system/bin/sh":                         true,
		"/bin/sh":                                true,
	}
	if got := androidShell(func(p string) bool { return present[p] }); got != "/system/bin/sh" {
		t.Fatalf("got %q want /system/bin/sh", got)
	}
}

// Pre-Android-11 devices have no /bin at all; the fallback must still be a
// shell that exists everywhere.
func TestAndroidShellFallsBackWhenNothingStats(t *testing.T) {
	if got := androidShell(func(string) bool { return false }); got != "/system/bin/sh" {
		t.Fatalf("got %q want /system/bin/sh", got)
	}
}
