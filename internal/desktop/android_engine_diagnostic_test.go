package desktop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"wanctl/internal/protocol"
)

// Engine stderr reaches the controller only as the engine's own error-code
// line, bounded and escaped; for any other stderr the error says it was not
// forwarded.
func TestAndroidStartForwardsOnlyEngineErrorCodes(t *testing.T) {
	for _, tc := range []struct{ name, stderr, want, never string }{
		{"private text", "SYNTHETIC_PRIVATE_CONTENT\n", "engine stderr not forwarded: its first line is not in the engine's error-code form", "SYNTHETIC_PRIVATE_CONTENT"},
		{"exception", "java.lang.SecurityException: SYNTHETIC_PRIVATE_CONTENT\n", "engine stderr not forwarded: its first line is not in the engine's error-code form", "SYNTHETIC_PRIVATE_CONTENT"},
		{"lower-case code", "hidden: SYNTHETIC_PRIVATE_CONTENT\n", "engine stderr not forwarded: its first line is not in the engine's error-code form", "SYNTHETIC_PRIVATE_CONTENT"},
		{"engine start failure", "ENGINE_START_FAILED: java.lang.IllegalStateException: UiAutomation not connected\n\tat dev.opencu.android.Main.main(Main.java:50)\n", `engine stderr: "ENGINE_START_FAILED: java.lang.IllegalStateException: UiAutomation not connected"`, "Main.java"},
		{"code", "HIDDEN_API_MISSING: hidden API missing\x1b" + strings.Repeat("x", 700) + "\nSECOND_LINE_PRIVATE\n", `HIDDEN_API_MISSING: hidden API missing\x1b`, "SECOND_LINE_PRIVATE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, d, _ := androidHost(t, "")
			d.mu.Lock()
			d.startupDiagnostic = tc.stderr
			d.mu.Unlock()
			s := &Service{}
			defer s.Close()
			res := s.DoOpenCU(context.Background(), "owner", NewID(), protocol.DesktopRequest{Discover: true}, func(bool) bool { return true }, host.StartOpenCU)
			wire, _ := json.Marshal(res)
			if res.OpenCU == nil || res.OpenCU.Code != "OPENCU_START_FAILED" || !strings.Contains(res.Error, tc.want) || strings.Contains(string(wire), tc.never) || strings.ContainsRune(res.Error, '\x1b') || len(res.Error) > 650 {
				t.Fatalf("result %s", wire)
			}
		})
	}
}
