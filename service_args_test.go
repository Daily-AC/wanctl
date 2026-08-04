package main

import (
	"strings"
	"testing"
)

// A unit is what runs after a reboot, when nobody is at a terminal to supply
// flags. `service install` used to emit `agent --managed` and nothing else, so a
// device installed this way came back with the hostname as its name and with no
// portal admin fingerprint — and the only symptom of the latter is a 502 in the
// browser when someone clicks "trust" on the portal, because the agent refuses
// the portal's console session. These tests pin the arguments into the unit.

func TestServiceAgentArgsCarryIdentityAndTrust(t *testing.T) {
	extra, err := serviceAgentArgs("vpnbox", "SHA256:YI9YqAdl4Ztfna66hJqdJafqsNTUQbIu7H5KFtwQCLM=", "")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(extra, " ")
	want := "--name vpnbox --portal-fps SHA256:YI9YqAdl4Ztfna66hJqdJafqsNTUQbIu7H5KFtwQCLM="
	if got != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

// Omitting --mode is what lets the persisted mode (and therefore a portal-side
// switch) survive a restart. Baking one in is possible but must be explicit.
func TestServiceAgentArgsOmitModeByDefault(t *testing.T) {
	extra, err := serviceAgentArgs("dev", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range extra {
		if a == "--mode" {
			t.Fatalf("args = %v, want no --mode", extra)
		}
	}
	extra, err = serviceAgentArgs("", "", "bypass")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(extra, " ") != "--mode bypass" {
		t.Fatalf("args = %v, want [--mode bypass]", extra)
	}
	if _, err := serviceAgentArgs("", "", "yolo"); err == nil {
		t.Fatal("--mode yolo accepted, want rejection")
	}
}

func TestServiceAgentArgsRejectMalformedFingerprint(t *testing.T) {
	if _, err := serviceAgentArgs("", "not-a-fingerprint", ""); err == nil {
		t.Fatal("malformed --portal-fps accepted; the unit would install and then fail at runtime")
	}
}

// systemd splits ExecStart on whitespace unless an argument is quoted, so an
// unquoted `--name lab box` would reach the agent as two arguments.
func TestSystemdArgsQuoteWhitespace(t *testing.T) {
	got := systemdArgs([]string{"--name", "lab box"})
	if got != ` --name "lab box"` {
		t.Fatalf("systemdArgs = %q, want ` --name \"lab box\"`", got)
	}
	if got := systemdArgs(nil); got != "" {
		t.Fatalf("systemdArgs(nil) = %q, want empty", got)
	}
}

func TestXMLEscapeKeepsPlistWellFormed(t *testing.T) {
	if got := xmlEscape(`a&b<c>"d"`); got != "a&amp;b&lt;c&gt;&quot;d&quot;" {
		t.Fatalf("xmlEscape = %q", got)
	}
}
