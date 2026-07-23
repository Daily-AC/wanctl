package agent

import (
	"testing"

	"wanctl/internal/protocol"
	"wanctl/internal/sessionauth"
)

func TestRequiredCapability(t *testing.T) {
	tests := []struct {
		kind string
		want sessionauth.Capabilities
	}{
		{protocol.KindExec, sessionauth.Exec},
		{protocol.KindExecAsync, sessionauth.Exec},
		{protocol.KindExecPoll, sessionauth.Exec},
		{protocol.KindFileGet, sessionauth.Read},
		{protocol.KindFilePut, sessionauth.Write},
		{protocol.KindLogs, sessionauth.Logs},
	}
	for _, tt := range tests {
		if got := requiredCapability(tt.kind); got != tt.want {
			t.Errorf("requiredCapability(%q) = %q, want %q", tt.kind, got, tt.want)
		}
	}
}
