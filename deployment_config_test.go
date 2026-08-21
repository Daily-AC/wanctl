package main

import (
	"context"
	"strings"
	"testing"

	"wanctl/internal/config"
)

func TestRelayCommandsFailBeforeNetworkWhenUnconfigured(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", "")
	t.Setenv("WANCTL_RELEASE_BASE", "")
	old := config.DefaultRelay
	config.DefaultRelay = ""
	t.Cleanup(func() { config.DefaultRelay = old })

	for name, run := range map[string]func() error{
		"docs list": func() error { return docsList(context.Background(), nil) },
		"update":    func() error { return fetchAndroidAPK(context.Background(), t.TempDir()) },
	} {
		t.Run(name, func(t *testing.T) {
			err := run()
			if err == nil || !strings.Contains(err.Error(), "wanctl config set relay=") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
