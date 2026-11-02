package client

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"wanctl/internal/desktop"
	"wanctl/internal/protocol"
)

func TestAndroidOpenCUClientForwardsDiscoveryAndDynamicTools(t *testing.T) {
	for _, req := range []protocol.DesktopRequest{{Discover: true}, {Tool: "android_dynamic_tool", Arguments: json.RawMessage(`{"id":9007199254740993}`)}} {
		ctl, dev := net.Pipe()
		done := make(chan error, 1)
		go func() {
			defer dev.Close()
			m, err := protocol.ReadMessage(dev)
			if err != nil {
				done <- err
				return
			}
			if m.Action != "act" || m.Desktop.Discover != req.Discover || m.Desktop.Tool != req.Tool || string(m.Desktop.Arguments) != string(req.Arguments) {
				t.Error("Android request changed")
			}
			result := protocol.OpenCUResult{Result: json.RawMessage(`{"content":[],"_meta":{"id":9007199254740993}}`)}
			if req.Discover {
				result = protocol.OpenCUResult{Discovery: json.RawMessage(`{"serverInfo":{"name":"opencu-android"},"tools":[]}`)}
			}
			done <- desktop.WriteResult(dev, protocol.DesktopResult{RequestID: m.RequestID, Status: "completed", OpenCU: &result}, nil)
		}()
		response, err := desktopCallWithDial(context.Background(), "android-phone", "act", "", req, func(context.Context) (net.Conn, error) { return ctl, nil })
		if err != nil || response == nil || response.Result.Status != "completed" {
			t.Fatalf("client refused Android: %+v %v", response, err)
		}
		if err = <-done; err != nil {
			t.Fatal(err)
		}
	}
}
