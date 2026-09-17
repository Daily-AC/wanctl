package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Unbinding has to reach the in-process trust stores, or a reinstalled device
// stays unreachable from the hosted MCP server until the relay restarts
// (ADR 0002). Registration deliberately does not: a device clearing its own
// identity alarm is the alternative that ADR rejected.
func TestUnbindingADeviceForgetsItsPin(t *testing.T) {
	r := New(envTokens{})
	r.SetAdminSecret("secret")
	r.SetAdmin(&noopAdmin{})
	var forgotten []string
	r.SetPinForgetter(func(ns, device string) { forgotten = append(forgotten, ns+"/"+device) })

	req := httptest.NewRequest(http.MethodPost, "/admin/devices/remove",
		strings.NewReader(`{"namespace":"alice","device":"build"}`))
	req.Header.Set("X-Admin-Secret", "secret")
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unbind returned %d: %s", rec.Code, rec.Body.String())
	}
	if len(forgotten) != 1 || forgotten[0] != "alice/build" {
		t.Fatalf("forgotten = %v, want one entry for alice/build", forgotten)
	}
}

// A rejected unbind must not drop the pin: the device is still bound.
func TestARefusedUnbindKeepsThePin(t *testing.T) {
	r := New(envTokens{})
	r.SetAdminSecret("secret")
	r.SetAdmin(&noopAdmin{})
	forgot := false
	r.SetPinForgetter(func(string, string) { forgot = true })

	for name, req := range map[string]*http.Request{
		"no admin secret": httptest.NewRequest(http.MethodPost, "/admin/devices/remove",
			strings.NewReader(`{"namespace":"alice","device":"build"}`)),
		"no device": httptest.NewRequest(http.MethodPost, "/admin/devices/remove",
			strings.NewReader(`{"namespace":"alice"}`)),
	} {
		if name == "no device" {
			req.Header.Set("X-Admin-Secret", "secret")
		}
		rec := httptest.NewRecorder()
		r.Handler().ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Fatalf("%s: unbind was accepted (%d)", name, rec.Code)
		}
	}
	if forgot {
		t.Fatal("a refused unbind still forgot the pin")
	}
}
