package agent

import (
	"testing"

	"wanctl/internal/protocol"
)

// TestLanSetRPC verifies the console RPC flips + persists the LAN switch and
// that console state snapshots expose it.
func TestLanSetRPC(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	a, err := New(Options{RelayURL: "ws://wan.example", Token: "t", Name: "dev1", LanRelay: "ws://***REMOVED-IP***:8080"})
	if err != nil {
		t.Fatal(err)
	}
	st := a.console.State()
	if st.Lan == nil || !st.Lan.Enabled || st.Lan.Connected {
		t.Fatalf("initial lan state wrong: %+v", st.Lan)
	}
	if st.Lan.Relay != "ws://***REMOVED-IP***:8080" {
		t.Fatalf("lan relay not exposed: %+v", st.Lan)
	}

	resp := a.handleConsoleRPC(protocol.Message{Kind: protocol.KindLanSet, Verdict: "off"})
	if resp.Kind != protocol.KindLanSet || len(resp.Data) > 0 {
		t.Fatalf("lan_set off failed: %+v", resp)
	}
	if st := a.console.State(); st.Lan.Enabled {
		t.Fatal("switch did not turn off")
	}

	// Persistence: a fresh agent in the same config dir starts with the switch off.
	b, err := New(Options{RelayURL: "ws://wan.example", Token: "t", Name: "dev1", LanRelay: "ws://***REMOVED-IP***:8080"})
	if err != nil {
		t.Fatal(err)
	}
	if st := b.console.State(); st.Lan.Enabled {
		t.Fatal("lan switch not persisted across restarts")
	}

	resp = b.handleConsoleRPC(protocol.Message{Kind: protocol.KindLanSet, Verdict: "on"})
	if resp.Kind != protocol.KindLanSet || len(resp.Data) > 0 {
		t.Fatalf("lan_set on failed: %+v", resp)
	}
	if st := b.console.State(); !st.Lan.Enabled {
		t.Fatal("switch did not turn back on")
	}
}

// TestLanSetRPCWithoutLanRelay verifies the RPC reports an error when the
// device has no LAN relay configured (and state carries no lan block).
func TestLanSetRPCWithoutLanRelay(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	a, err := New(Options{RelayURL: "ws://wan.example", Token: "t", Name: "dev1"})
	if err != nil {
		t.Fatal(err)
	}
	if st := a.console.State(); st.Lan != nil {
		t.Fatalf("lan state should be absent, got %+v", st.Lan)
	}
	resp := a.handleConsoleRPC(protocol.Message{Kind: protocol.KindLanSet, Verdict: "on"})
	if len(resp.Data) == 0 {
		t.Fatal("expected an error payload when no LAN relay is configured")
	}
}
