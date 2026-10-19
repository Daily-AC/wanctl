package client

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wanctl/internal/admission"
	"wanctl/internal/desktop"
	"wanctl/internal/httpconn"
	"wanctl/internal/opencu"
	"wanctl/internal/opencu/ocutest"
	"wanctl/internal/protocol"
	"wanctl/internal/relay"
	"wanctl/internal/sessionauth"
	"wanctl/internal/wsconn"
)

func TestOpenCULargeResultThroughRelayCarriers(t *testing.T) {
	binary := ocutest.Build(t)
	for _, carrier := range []string{"ws", "http"} {
		t.Run(carrier, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			peer, err := opencu.Start(ctx, exec.Command(binary))
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			srv := httptest.NewServer(relay.New(relay.EnvTokenStore("opencu-test:alice")).Handler())
			defer srv.Close()
			base := "ws" + strings.TrimPrefix(srv.URL, "http")
			control, _, err := wsconn.Dial(ctx, base+"/agent", admission.Header("opencu-test"))
			if err != nil {
				t.Fatal(err)
			}
			defer control.Close()
			json.NewEncoder(control).Encode(map[string]string{"op": "register", "device": "opencu-lab"})
			expected := make(chan json.RawMessage, 1)
			served := make(chan error, 1)
			go func() {
				var open sessionauth.Open
				if err := json.NewDecoder(control).Decode(&open); err != nil {
					served <- err
					return
				}
				device, _, err := wsconn.Dial(ctx, base+open.URL, admission.Header("opencu-test"))
				if err != nil {
					served <- err
					return
				}
				defer device.Close()
				req, err := protocol.ReadMessage(device)
				if err != nil {
					served <- err
					return
				}
				raw, err := peer.Call(ctx, req.Desktop.Tool, req.Desktop.Arguments)
				if err != nil {
					served <- err
					return
				}
				expected <- raw
				err = desktop.WriteResult(device, protocol.DesktopResult{RequestID: req.RequestID, Status: "completed", OpenCU: &protocol.OpenCUResult{Result: raw}}, nil)
				if err == nil {
					var b [1]byte
					device.Read(b[:])
				} // Keep carrier open until controller receives the tail.
				served <- err
			}()
			// Wait for this isolated relay to acknowledge registration.
			for {
				req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/peers", nil)
				admission.SetBearer(req, "opencu-test")
				resp, e := http.DefaultClient.Do(req)
				if e != nil {
					t.Fatal(e)
				}
				var data any
				json.NewDecoder(resp.Body).Decode(&data)
				resp.Body.Close()
				encoded, _ := json.Marshal(data)
				if bytes.Contains(encoded, []byte("opencu-lab")) {
					break
				}
				if ctx.Err() != nil {
					t.Fatal(ctx.Err())
				}
				time.Sleep(time.Millisecond)
			}
			var dials atomic.Int32
			dial := func(ctx context.Context) (net.Conn, error) {
				dials.Add(1)
				if carrier == "ws" {
					c, _, e := wsconn.Dial(ctx, base+"/dial?target=alice/opencu-lab", admission.Header("opencu-test"))
					return c, e
				}
				req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/h/dial?target=alice/opencu-lab", nil)
				admission.SetBearer(req, "opencu-test")
				resp, e := http.DefaultClient.Do(req)
				if e != nil {
					return nil, e
				}
				defer resp.Body.Close()
				var open sessionauth.Open
				if e = json.NewDecoder(resp.Body).Decode(&open); e != nil {
					return nil, e
				}
				return httpconn.Dial(ctx, srv.URL, open.Session, "client", "opencu-test")
			}
			res, err := desktopCallWithDial(ctx, "alice/opencu-lab", "act", "", protocol.DesktopRequest{Tool: "peer_apply", Arguments: json.RawMessage(`{"large":true,"error":true,"app":"Disposable"}`)}, dial)
			if err != nil {
				t.Fatal(err)
			}
			raw := <-expected
			if len(raw) <= protocol.MaxFrame || !bytes.Equal(raw, res.Result.OpenCU.Result) || dials.Load() != 1 {
				t.Fatal("large relay result changed or request replayed")
			}
			if err := <-served; err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestOpenCUClientBreakNeverRedials(t *testing.T) {
	var dials atomic.Int32
	dial := func(context.Context) (net.Conn, error) {
		dials.Add(1)
		ctl, dev := net.Pipe()
		go func() { defer dev.Close(); protocol.ReadMessage(dev) }()
		return ctl, nil
	}
	_, err := desktopCallWithDial(context.Background(), "device", "act", "", protocol.DesktopRequest{Tool: "arbitrary"}, dial)
	if err == nil || !strings.Contains(err.Error(), "observe again") || dials.Load() != 1 {
		t.Fatalf("%v dials=%d", err, dials.Load())
	}
}
