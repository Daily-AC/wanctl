package portal

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wanctl/internal/console"
	"wanctl/internal/protocol"
	"wanctl/internal/transport"
)

func TestADBPairUsesOwnedConsoleAndTypedRequest(t *testing.T) {
	s := newTestPortal(relayFor("alice", "phone", transport.Fingerprint([]byte("phone"))))
	withDialer(t, s)
	local, remote := net.Pipe()
	d := newDeviceConn(local)
	defer d.close()
	defer remote.Close()
	s.conns["alice/phone"] = d
	requests := make(chan protocol.Message, 1)
	go func() {
		m, err := protocol.ReadMessage(remote)
		if err != nil {
			return
		}
		if m.Kind != protocol.KindConsoleState {
			return
		}
		state, _ := json.Marshal(console.State{Info: console.Info{Platform: "android", ADBPair: true}})
		protocol.WriteMessage(remote, protocol.Message{Kind: protocol.KindConsoleState, Data: state})
		m, err = protocol.ReadMessage(remote)
		if err != nil {
			return
		}
		requests <- m
		protocol.WriteMessage(remote, protocol.Message{Kind: protocol.KindADBPair})
	}()
	rec := httptest.NewRecorder()
	s.handleDeviceADBPair(rec, userReq("POST", "/api/devices/adb-pair", map[string]any{"device": "phone", "port": 37129, "code": "012345"}))
	if rec.Code != 200 {
		t.Fatalf("pair status %d: %s", rec.Code, rec.Body.String())
	}
	m := <-requests
	if m.Kind != protocol.KindADBPair || m.PairPort != 37129 || m.PairCode != "012345" || m.Command != "" {
		t.Fatalf("unexpected wire request: kind=%s port=%d", m.Kind, m.PairPort)
	}
	if strings.TrimSpace(rec.Body.String()) != `{"paired":true}` {
		t.Fatalf("unexpected result: %s", rec.Body.String())
	}
}

func TestADBPairRejectsSharedDeviceInvalidInputAndCSRF(t *testing.T) {
	owner := newTestPortal(relayFor("alice", "phone", transport.Fingerprint([]byte("phone"))))
	for _, input := range []map[string]any{
		{"device": "phone", "port": 0, "code": "123456"},
		{"device": "phone", "port": 65536, "code": "123456"},
		{"device": "phone", "port": 37129, "code": "1;id"},
		{"device": "phone", "port": 37129, "code": "１２３４５６"},
	} {
		rec := httptest.NewRecorder()
		owner.handleDeviceADBPair(rec, userReq("POST", "/api/devices/adb-pair", input))
		if rec.Code != 400 {
			t.Fatalf("invalid input reached dial: %d", rec.Code)
		}
	}
	shared := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/admin/resolve-user" {
			json.NewEncoder(w).Encode(map[string]string{"namespace": "bob"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{{"name": "phone", "owner": "alice", "shared": true}}})
	})
	rec := httptest.NewRecorder()
	shared.handleDeviceADBPair(rec, userReq("POST", "/api/devices/adb-pair", map[string]any{"device": "phone", "port": 37129, "code": "123456"}))
	if rec.Code != 403 {
		t.Fatalf("shared device paired: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	owner.Handler().ServeHTTP(rec, userReq("POST", "/api/devices/adb-pair", map[string]any{}))
	if rec.Code != 403 {
		t.Fatalf("missing CSRF accepted: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	owner.Handler().ServeHTTP(rec, userReq("GET", "/api/devices/adb-pair", nil))
	if rec.Code != 405 {
		t.Fatalf("GET allowed mutation: %d", rec.Code)
	}
}
