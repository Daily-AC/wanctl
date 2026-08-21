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
	extra, err := serviceAgentArgs("vpnbox", "SHA256:YI9YqAdl4Ztfna66hJqdJafqsNTUQbIu7H5KFtwQCLM=", "", "https://relay.example.org", "ws")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(extra, " ")
	want := "--name vpnbox --portal-fps SHA256:YI9YqAdl4Ztfna66hJqdJafqsNTUQbIu7H5KFtwQCLM= --relay https://relay.example.org --transport ws"
	if got != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
}

// Release binaries ship without a baked-in relay, and service managers do not
// read the shell profile the user exported WANCTL_RELAY into, so a unit
// without --relay is dead on arrival after a reboot (issue #2). The relay is
// therefore mandatory, and the transport rides along so an agent enrolled over
// ws does not silently fall back to http on restart.
func TestServiceAgentArgsRequireRelay(t *testing.T) {
	if _, err := serviceAgentArgs("dev", "", "", "", "http"); err == nil {
		t.Fatal("empty relay accepted; the unit would install and then die at every boot")
	}
}

func TestServiceAgentArgsRejectUnknownTransport(t *testing.T) {
	if _, err := serviceAgentArgs("", "", "", "https://relay.example.org", "carrier-pigeon"); err == nil {
		t.Fatal("bogus --transport accepted, want rejection")
	}
}

// Omitting --mode is what lets the persisted mode (and therefore a portal-side
// switch) survive a restart. Baking one in is possible but must be explicit.
func TestServiceAgentArgsOmitModeByDefault(t *testing.T) {
	extra, err := serviceAgentArgs("dev", "", "", "https://relay.example.org", "http")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range extra {
		if a == "--mode" {
			t.Fatalf("args = %v, want no --mode", extra)
		}
	}
	extra, err = serviceAgentArgs("", "", "bypass", "https://relay.example.org", "http")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(extra, " ") != "--mode bypass --relay https://relay.example.org --transport http" {
		t.Fatalf("args = %v", extra)
	}
	if _, err := serviceAgentArgs("", "", "yolo", "https://relay.example.org", "http"); err == nil {
		t.Fatal("--mode yolo accepted, want rejection")
	}
}

func TestServiceAgentArgsRejectMalformedFingerprint(t *testing.T) {
	if _, err := serviceAgentArgs("", "not-a-fingerprint", "", "https://relay.example.org", "http"); err == nil {
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
