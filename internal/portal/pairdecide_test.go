package portal

import (
	"encoding/json"
	"net"
	"net/http/httptest"
	"strings"
	"testing"

	"wanctl/internal/console"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
	"wanctl/internal/transport"
)

// pairDecideDevice plays the agent side of one pair_decide RPC and replies with
// the shape internal/agent actually sends when console.DecidePair says no: the
// same kind back, with the reason parked in Data. That reply used to read as
// success all the way to the browser.
func pairDecideDevice(t *testing.T, nc net.Conn, reason string) <-chan protocol.Message {
	t.Helper()
	got := make(chan protocol.Message, 1)
	go func() {
		m, err := protocol.ReadMessage(nc)
		if err != nil {
			return
		}
		got <- m
		resp := protocol.Message{Kind: protocol.KindPairDecide}
		if reason != "" {
			b, _ := json.Marshal(reason)
			resp.Data = json.RawMessage(b)
		}
		protocol.WriteMessage(nc, resp)
	}()
	return got
}

// The device rejects a verdict for a fingerprint it no longer holds. That has to
// come back as an error, not as a silent success (issue #79).
func TestPairDecideSurfacesTheDeviceRejection(t *testing.T) {
	cli, srv := net.Pipe()
	defer srv.Close()
	d := newDeviceConn(cli)
	defer d.close()
	pairDecideDevice(t, srv, "no such pending pairing")

	if err := d.pairDecide("SHA256:stale", "y"); err == nil {
		t.Fatal("pairDecide reported success for a pairing the device had dropped")
	}
}

func TestPairDecideStaysQuietOnSuccess(t *testing.T) {
	cli, srv := net.Pipe()
	defer srv.Close()
	d := newDeviceConn(cli)
	defer d.close()
	pairDecideDevice(t, srv, "")

	if err := d.pairDecide("SHA256:live", "y"); err != nil {
		t.Fatalf("pairDecide on a live request: %v", err)
	}
}

// End to end through the handler: a verdict on an expired card must reach the
// browser as 404 pairing_gone, which the SPA turns into one sentence. 502 would
// read as "the device is unreachable", which is a different thing to do about.
func TestDevicePairReportsGoneRequestAs404(t *testing.T) {
	s := newTestPortal(relayFor("alice", "legion", transport.Fingerprint([]byte("legion"))))
	withDialer(t, s)
	cli, srv := net.Pipe()
	defer srv.Close()
	d := newDeviceConn(cli)
	defer d.close()
	s.conns["alice/legion"] = d
	pairDecideDevice(t, srv, "no such pending pairing")

	rec := httptest.NewRecorder()
	s.handleDevicePair(rec, userReq("POST", "/api/devices/pair",
		map[string]any{"device": "legion", "fp": "SHA256:stale", "verdict": "y"}))

	if rec.Code != 404 {
		t.Fatalf("status = %d %q; want 404", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "pairing_gone" {
		t.Fatalf("body = %q; want the pairing_gone token the SPA translates", got)
	}
}

func TestDevicePairStillReports200OnSuccess(t *testing.T) {
	s := newTestPortal(relayFor("alice", "legion", transport.Fingerprint([]byte("legion"))))
	withDialer(t, s)
	cli, srv := net.Pipe()
	defer srv.Close()
	d := newDeviceConn(cli)
	defer d.close()
	s.conns["alice/legion"] = d
	pairDecideDevice(t, srv, "")

	rec := httptest.NewRecorder()
	s.handleDevicePair(rec, userReq("POST", "/api/devices/pair",
		map[string]any{"device": "legion", "fp": "SHA256:live", "verdict": "y"}))

	if rec.Code != 200 {
		t.Fatalf("status = %d %q; want 200", rec.Code, rec.Body.String())
	}
}

// The pairing deep link decides what to draw from the device's own console
// snapshot: a fingerprint still in pending_pairings is answerable, one already
// in trusted is done, and one in neither is gone. That is three different
// screens read off two fields of one existing response, so those two fields
// have to survive the trip through the handler with their fingerprints intact.
// Dropping either one silently sends every consumed link back to showing two
// buttons over a request the device no longer holds.
func TestDeviceConsoleCarriesPendingAndTrustedFingerprints(t *testing.T) {
	s := newTestPortal(relayFor("alice", "legion", transport.Fingerprint([]byte("legion"))))
	withDialer(t, s)
	cli, srv := net.Pipe()
	defer srv.Close()
	d := newDeviceConn(cli)
	defer d.close()
	s.conns["alice/legion"] = d

	go func() {
		m, err := protocol.ReadMessage(srv)
		if err != nil || m.Kind != protocol.KindConsoleState {
			return
		}
		b, _ := json.Marshal(console.State{
			Mode:            policy.ModeNormal,
			PendingPairings: []console.PendingPairing{{FP: "SHA256:waiting", Name: "kestrel", Label: "claude-code on kestrel"}},
			Trusted:         []console.TrustedController{{FP: "SHA256:already", Name: "studio", Label: "studio (my laptop)"}},
		})
		protocol.WriteMessage(srv, protocol.Message{Kind: protocol.KindConsoleState, Data: b})
	}()

	rec := httptest.NewRecorder()
	s.handleDeviceConsole(rec, userReq("GET", "/api/devices/console?device=legion", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d %q; want 200", rec.Code, rec.Body.String())
	}

	var got struct {
		PendingPairings []struct{ FP string } `json:"pending_pairings"`
		Trusted         []struct{ FP string } `json:"trusted"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if len(got.PendingPairings) != 1 || got.PendingPairings[0].FP != "SHA256:waiting" {
		t.Fatalf("pending_pairings = %+v; want the one waiting fingerprint", got.PendingPairings)
	}
	if len(got.Trusted) != 1 || got.Trusted[0].FP != "SHA256:already" {
		t.Fatalf("trusted = %+v; want the one trusted fingerprint", got.Trusted)
	}
}
