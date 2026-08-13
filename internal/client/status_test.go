package client

import (
	"encoding/json"
	"strings"
	"testing"

	"wanctl/internal/protocol"
)

func TestDecodeStatusFromOlderAgent(t *testing.T) {
	tests := []protocol.Message{
		{Kind: protocol.KindError, Reason: "unknown request: status"},
		{Kind: protocol.KindError, Data: json.RawMessage(`"unknown RPC kind"`)},
	}
	for _, reply := range tests {
		got, err := decodeStatus(reply)
		if err != nil {
			t.Fatalf("decodeStatus(%+v): %v", reply, err)
		}
		if got.Detailed {
			t.Fatalf("older agent status = %+v, want no details", got)
		}
	}
}

func TestDecodeStatusRejectsInvalidResponse(t *testing.T) {
	_, err := decodeStatus(protocol.Message{Kind: protocol.KindStatus, ConsoleMode: "unsafe"})
	if err == nil || !strings.Contains(err.Error(), "invalid policy mode") {
		t.Fatalf("invalid mode error = %v", err)
	}
	_, err = decodeStatus(protocol.Message{Kind: protocol.KindError, Reason: "status unavailable"})
	if err == nil || !strings.Contains(err.Error(), "status unavailable") {
		t.Fatalf("remote error = %v", err)
	}
}
