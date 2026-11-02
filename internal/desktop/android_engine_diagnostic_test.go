package desktop

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"wanctl/internal/protocol"
)

// Engine stderr reaches the controller only as the engine's own error-code
// line, bounded and escaped; any other stderr is replaced by a fixed message.
func TestAndroidStartForwardsOnlyEngineErrorCodes(t *testing.T) {
	for _, tc := range []struct{ name, stderr, want, never string }{
		{"private text", "SYNTHETIC_PRIVATE_CONTENT\n", "engine diagnostics stay on the device", "SYNTHETIC_PRIVATE_CONTENT"},
		{"exception", "java.lang.SecurityException: SYNTHETIC_PRIVATE_CONTENT\n", "engine diagnostics stay on the device", "SYNTHETIC_PRIVATE_CONTENT"},
		{"lower-case code", "hidden: SYNTHETIC_PRIVATE_CONTENT\n", "engine diagnostics stay on the device", "SYNTHETIC_PRIVATE_CONTENT"},
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
