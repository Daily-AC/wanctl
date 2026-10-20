package relay

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDeviceVersionValidation(t *testing.T) {
	for _, value := range []string{"", "dev", "v1.2.3", "1.0.0-beta.2", "v1.0.0+abc_123", strings.Repeat("a", maxDeviceVersionBytes)} {
		if got := cleanDeviceVersion(value); got != value {
			t.Errorf("%q => %q", value, got)
		}
	}
	for _, value := range []string{strings.Repeat("a", maxDeviceVersionBytes+1), "v1.0.0\n", "<script>", "v1.0.0 bad", "版本1", "a/b", "a\x00b"} {
		if got := cleanDeviceVersion(value); got != "" {
			t.Errorf("accepted %q", value)
		}
	}
	for _, raw := range []string{`123`, `null`, `{}`, `[]`, `true`} {
		if got := jsonVersion(json.RawMessage(raw)); got != "" {
			t.Errorf("accepted JSON %s", raw)
		}
	}
}

func TestDeviceVersionComparison(t *testing.T) {
	for _, tt := range []struct{ installed, stable, want string }{
		{"v1.2.3", "v1.2.3", "latest"},
		{"v1.2.2", "v1.2.3", "behind"},
		{"v1.2.3-beta.1", "v1.2.3", "behind"},
		{"v1.3.0-beta.1", "v1.2.3", "ahead"},
		{"v1.3.0", "v1.2.3", "ahead"},
		{"v1.2.9", "v1.2.10", "behind"},
		{"", "v1.2.3", "unknown"},
		{"dev", "v1.2.3", "unknown"},
		{"garbage", "v1.2.3", "unknown"},
		{"v1.2.3", "", "unknown"},
	} {
		if got := deviceVersionStatus(tt.installed, tt.stable); got != tt.want {
			t.Errorf("%s / %s = %s, want %s", tt.installed, tt.stable, got, tt.want)
		}
	}
}
