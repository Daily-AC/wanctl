package main

import (
	"context"
	"strings"
	"testing"

	"wanctl/internal/config"
)

func TestRelayCommandsFailBeforeNetworkWhenUnconfigured(t *testing.T) {
	t.Setenv("WANCTL_RELAY", "")
	old := config.DefaultRelay
	config.DefaultRelay = ""
	t.Cleanup(func() { config.DefaultRelay = old })

	for name, run := range map[string]func() error{
		"docs list": func() error { return docsList(context.Background(), nil) },
		"update":    func() error { return fetchAndroidAPK(context.Background(), t.TempDir()) },
	} {
		t.Run(name, func(t *testing.T) {
			err := run()
			if err == nil || !strings.Contains(err.Error(), "set WANCTL_RELAY=https://your-relay") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
