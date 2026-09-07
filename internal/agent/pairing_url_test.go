package agent

import (
	"strings"
	"testing"

	"wanctl/internal/config"
)

func TestPairingURLRequiresPortal(t *testing.T) {
	// Isolate the config directory: clearing the environment and the build
	// default still leaves `wanctl config set portal=…`, so on a developer's
	// own machine the "no portal anywhere" case read a real portal and the
	// test failed.
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
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
