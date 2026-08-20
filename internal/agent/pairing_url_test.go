package agent

import (
	"strings"
	"testing"

	"wanctl/internal/config"
)

func TestPairingURLRequiresPortal(t *testing.T) {
	old := config.DefaultPortal
	config.DefaultPortal = ""
	t.Cleanup(func() { config.DefaultPortal = old })
	t.Setenv("WANCTL_PORTAL", "")
	a := &Agent{opts: Options{Name: "device-one"}}
	if got := a.pairingURL("SHA256:controller", "controller", "operator"); got != "" {
		t.Fatalf("pairingURL = %q, want empty", got)
	}

	t.Setenv("WANCTL_PORTAL", "https://portal.example/")
	got := a.pairingURL("SHA256:controller", "controller", "operator")
	if !strings.HasPrefix(got, "https://portal.example/#pair?") {
		t.Fatalf("pairingURL = %q", got)
	}
}
