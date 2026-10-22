package main

import (
	"os"
	"strings"
	"testing"
)

func TestInvariant8OldAgentAdvice(t *testing.T) {
	p := autoUpdateProbe{enabled: func() bool { return false }, version: "v1.0.0"}
	if decideAutoUpdate(t.Context(), p).action != autoUpdateSkip {
		t.Fatal("disabled auto updater unexpectedly active")
	}
	for _, path := range []string{"internal/portal/install.go", "docs/adr/0019-portal-triggered-installs.md", "docs/portal/control__control-device.en.md", "docs/portal/control__control-device.md"} {
		b, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		s := string(b)
		if strings.Contains(s, "will update itself within six hours") || strings.Contains(s, "within six hours") || strings.Contains(s, "six-hour automatic") || strings.Contains(s, "六小时内的自动更新") {
			t.Errorf("%s promises a six-hour automatic upgrade, but auto_update=off provably skips forever (also scheduler adds up to 30m jitter/busy retry)", path)
		}
	}
}
