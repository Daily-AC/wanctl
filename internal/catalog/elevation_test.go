package catalog

import (
	"strings"
	"testing"
)

// TestExecDescribesElevationAsItIs holds the --elevate text to the rule the
// device has enforced since #108, and to the failures only a phone's owner can
// fix.
//
// This text matters more than most: the controller updates itself and the
// Android app does not, so for a phone still running an old app it is the only
// explanation anyone gets. The flag used to say bypass mode refuses elevated
// commands until a human approves — true before #108, the opposite of the rule
// wanctl_screenshot describes since. And the two adb failures ended in advice
// the owner could not act on: "turn on Wireless debugging" to someone whose
// wireless debugging Android had quietly switched off, or who had it on and
// was looking at an app that never found its port.
func TestExecDescribesElevationAsItIs(t *testing.T) {
	exec, ok := Lookup("exec")
	if !ok {
		t.Fatal("no exec entry")
	}
	var elevate string
	for _, p := range exec.Params {
		if p.Name == "elevate" {
			elevate = p.Desc
		}
	}
	for _, want := range []string{
		"bypass mode AND has its elevation channel switched on",
		"Bypass mode alone refuses them",
		"elevated command denied by device policy",
		// The markers below are what the device prints; MCP hosts never see
		// Errors, so the flag has to carry them too.
		"could not reach adbd on this device", "停用 then 启用",
		"TLS handshake with adbd", "remote error: tls", "ADB pairing",
	} {
		if !strings.Contains(elevate, want) {
			t.Errorf("--elevate no longer says %q", want)
		}
	}
	if strings.Contains(elevate, "bypass mode still refuses them") {
		t.Error("--elevate still describes the rule from before #108")
	}

	// internal/elevate and internal/adb produce these two texts, and their tests
	// assert them; an agent reading `wanctl help exec` matches on them.
	for _, text := range []string{"could not reach adbd on this device", "TLS handshake with adbd"} {
		if !Declares(text) {
			t.Errorf("exec does not declare the failure %q", text)
		}
	}
}
