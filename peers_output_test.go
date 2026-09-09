package main

import (
	"strings"
	"testing"

	"wanctl/internal/client"
)

// A grantee reads the owner namespace off this listing and pastes the whole
// target into --target; a bare label would be looked up in their own namespace.
func TestSharedPeerLinesPrintPastableTargets(t *testing.T) {
	got := sharedPeerLines([]client.SharedDevice{
		{Owner: "daily-ac", Device: "8e894048-2222-4333-8444-555566667777", Label: "bms-20558674",
			Target: "daily-ac/8e894048-2222-4333-8444-555566667777", Online: true},
		{Owner: "daily-ac", Device: "aa11", Label: "aa11", Target: "daily-ac/aa11"},
	})
	want := "\nshared with you (pass the whole owner/device to --target):\n" +
		"daily-ac/8e894048-2222-4333-8444-555566667777  (bms-20558674)\n" +
		"daily-ac/aa11  [offline]\n"
	if got != want {
		t.Fatalf("shared listing =\n%q\nwant\n%q", got, want)
	}
	if sharedPeerLines(nil) != "" {
		t.Fatalf("a token with no grants must print nothing extra: %q", sharedPeerLines(nil))
	}
	for _, line := range strings.Split(strings.TrimSpace(got), "\n")[1:] {
		if !strings.HasPrefix(line, "daily-ac/") {
			t.Fatalf("line is not a usable target: %q", line)
		}
	}
}
