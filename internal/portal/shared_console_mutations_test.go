package portal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sharedread_test.go covers the reads — console, logs, events, and the two
// owner-only settings. The routes that change the device are the ones worth
// getting wrong: approving a command, trusting a controller fingerprint,
// revoking one, editing the rule set, switching the mode, accepting a changed
// device identity, pairing ADB. Each is checked with the management switch off
// and on, so the table says what every route does in both states rather than
// leaving the mutation paths to be inferred from the read paths.
func TestSharedDeviceMutationRoutesFollowTheManagementSwitch(t *testing.T) {
	type route struct {
		path   string
		method string
		body   string
		// owner is true for routes a share never carries, however it is set.
		owner bool
	}
	// One body covers every handler: each reads only the fields it knows.
	const body = `{"device":"devbox","id":7,"verdict":"y","fp":"SHA256:kQ2p","op":"rm","index":0,` +
		`"kind":"exec","pattern":"*","mode":"normal","port":5555,"code":"123456"}`

	routes := []route{
		{"/api/devices/decide", http.MethodPost, body, false},
		{"/api/devices/pair", http.MethodPost, body, false},
		{"/api/devices/untrust", http.MethodPost, body, false},
		{"/api/devices/rules", http.MethodPost, body, false},
		{"/api/devices/mode", http.MethodPost, body, false},
		{"/api/devices/identity/accept", http.MethodPost, body, false},
		{"/api/devices/adb-pair", http.MethodPost, body, false},
		// The owner's contact details, not device state. Never a grantee's.
		{"/api/devices/lark", http.MethodPost, body, true},
		{"/api/devices/notify", http.MethodPost, body, true},
	}

	for _, manage := range []bool{false, true} {
		manage := manage
		name := "manage=false"
		if manage {
			name = "manage=true"
		}
		t.Run(name, func(t *testing.T) {
			for _, rt := range routes {
				rt := rt
				t.Run(strings.TrimPrefix(rt.path, "/api/devices/"), func(t *testing.T) {
					reached := ""
					s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
						switch r.URL.Path {
						case "/admin/resolve-user":
							json.NewEncoder(w).Encode(map[string]string{"namespace": "bob"})
						case "/admin/devices":
							json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{{
								"name": "devbox", "owner": "alice", "shared": true,
								"perms": "full", "manage": manage,
							}}})
						default:
							reached = r.URL.Path
							w.WriteHeader(http.StatusOK)
						}
					})
					h := map[string]http.HandlerFunc{
						"/api/devices/decide":          s.handleDeviceDecide,
						"/api/devices/pair":            s.handleDevicePair,
						"/api/devices/untrust":         s.handleDeviceUntrust,
						"/api/devices/rules":           s.handleDeviceRules,
						"/api/devices/mode":            s.handleDeviceMode,
						"/api/devices/identity/accept": s.handleDeviceIdentityAccept,
						"/api/devices/adb-pair":        s.handleDeviceADBPair,
						"/api/devices/lark":            s.handleDeviceLarkWrite,
						"/api/devices/notify":          s.handleDeviceNotify,
					}[rt.path]

					rec := httptest.NewRecorder()
					req := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
					req.Header.Set("X-User", "bob@example.com")
					h(rec, req)

					// Owner-only routes are refused in both states. Control-plane
					// routes are refused only without the switch.
					wantRefused := rt.owner || !manage
					if wantRefused {
						if rec.Code != http.StatusForbidden {
							t.Errorf("status = %d, want 403; body = %q", rec.Code, strings.TrimSpace(rec.Body.String()))
						}
						// A gate that answers 403 after already asking the relay
						// has still done the thing it refused to admit to.
						if reached != "" {
							t.Errorf("refused the caller but had already reached the relay at %s", reached)
						}
						return
					}
					if rec.Code == http.StatusForbidden {
						t.Errorf("status = 403 with the management switch on; body = %q",
							strings.TrimSpace(rec.Body.String()))
					}
				})
			}
		})
	}
}
