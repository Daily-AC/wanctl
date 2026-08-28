package portal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The Android enrollment dialog only collects a portal; the app derives the
// relay from this endpoint, so it must answer without a session (GH #3).
func TestInstanceEndpointAnswersWithoutSession(t *testing.T) {
	s := New(Config{RelayDialURL: "wss://relay.example:8443/", Transport: "ws"})
	rec := httptest.NewRecorder()
	s.handleInstance(rec, httptest.NewRequest("GET", "/api/instance", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	var got struct {
		Relay     string `json:"relay"`
		Transport string `json:"transport"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Relay != "https://relay.example:8443" || got.Transport != "ws" {
		t.Fatalf("got %+v", got)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q", cc)
	}
}

func TestInstanceEndpointRefusesWhenUnwiredOrNotGET(t *testing.T) {
	s := New(Config{})
	rec := httptest.NewRecorder()
	s.handleInstance(rec, httptest.NewRequest("GET", "/api/instance", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired: status = %d", rec.Code)
	}
	s = New(Config{RelayDialURL: "https://relay.example"})
	rec = httptest.NewRecorder()
	s.handleInstance(rec, httptest.NewRequest("POST", "/api/instance", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: status = %d", rec.Code)
	}
}
