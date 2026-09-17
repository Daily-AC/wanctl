package portal

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"wanctl/internal/delegation"
	"wanctl/internal/transport"
)

const testDelegationID = "request_test_1234"

func TestWebFetchHelpIsPublicAndDoesNotContactRelay(t *testing.T) {
	s := newOAuthPortal(t, func(_ map[string]string, _ http.ResponseWriter) {
		t.Fatal("public help must not resolve an account")
	})
	s.relayPublic = "https://public-relay.test"
	s.hc = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("public help must not contact the relay")
		return nil, nil
	})}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/webfetch/help", nil))
	body := w.Body.String()
	if w.Code != http.StatusOK || w.Header().Get("Location") != "" {
		t.Fatalf("anonymous help = %d %s", w.Code, w.Header().Get("Location"))
	}
	for _, want := range []string{"CALL_ENDPOINT?rid={rid}", "tool=exec", "call_url_template", "result_url", "pairing_required", "NEW rid", "https://public-relay.test/webfetch/v1"} {
		if !strings.Contains(body, want) {
			t.Errorf("server-rendered help missing %q", want)
		}
	}
	for _, secret := range []string{"/webfetch/s/", "wfd_", "client123", "secret456"} {
		if strings.Contains(body, secret) {
			t.Fatalf("public help contains session or operator data: %s", secret)
		}
	}
}

func TestWebFetchConnectUsesIndependentBootstrapURLsWithoutGrantingAccess(t *testing.T) {
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "alice", "role": "user"})
		case "/webfetch/v1":
			json.NewEncoder(w).Encode(map[string]string{"protocol": "wanctl.webfetch.v1", "start_url_template": "https://relay.test/webfetch/new/{client_nonce}"})
		default:
			t.Fatalf("starter page must not create or approve a grant: %s", r.URL.Path)
		}
	})
	pattern := regexp.MustCompile(`https://relay\.test/webfetch/new/[a-f0-9]{48}`)
	previous := ""
	for i := 0; i < 2; i++ {
		r := httptest.NewRequest(http.MethodGet, "/webfetch/connect", nil)
		r.Header.Set("X-User", "alice@example.com")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		start := pattern.FindString(w.Body.String())
		if w.Code != 200 || start == "" || start == previous || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("starter page reused or omitted its fresh URL: %d", w.Code)
		}
		if strings.Contains(w.Body.String(), "/webfetch/s/") || strings.Contains(w.Body.String(), "wfd_") || !strings.Contains(w.Body.String(), `id="webfetchCopy"`) {
			t.Fatal("starter page exposed a credential or omitted its copy action")
		}
		if !strings.Contains(w.Body.String(), "http://example.com/webfetch/help") || !strings.Contains(w.Body.String(), "exec call_url_template") {
			t.Fatal("starter prompt lost its help URL or cross-turn call instructions")
		}
		previous = start
	}
}

func TestWebFetchConnectRequiresOwnerLogin(t *testing.T) {
	s := newOAuthPortal(t, resolveOKAs("alice", "user"))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/webfetch/connect", nil))
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "next=%2Fwebfetch%2Fconnect") {
		t.Fatalf("unauthenticated starter page = %d %s", w.Code, w.Header().Get("Location"))
	}
}

func delegationFixture() delegation.Request {
	return delegation.Request{ID: testDelegationID, Label: "Qwen experiment", Status: "pending",
		ControllerFingerprint: transport.Fingerprint([]byte("temporary controller")),
		CreatedAt:             time.Now(), RequestExpiresAt: time.Now().Add(10 * time.Minute)}
}

func delegationPortal(t *testing.T, request delegation.Request, devices []delegationDevice, onApprove func(map[string]any)) *Server {
	t.Helper()
	return newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "alice", "role": "user"})
		case "/admin/delegations/request":
			if r.URL.Query().Get("namespace") != "alice" || r.URL.Query().Get("id") != testDelegationID {
				t.Errorf("request not owner scoped: %s", r.URL)
			}
			json.NewEncoder(w).Encode(request)
		case "/admin/devices":
			if r.URL.Query().Get("namespace") != "alice" {
				t.Errorf("devices not owner scoped: %s", r.URL)
			}
			json.NewEncoder(w).Encode(map[string]any{"devices": devices})
		case "/admin/delegations/approve", "/admin/delegations/reject":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			if onApprove != nil {
				onApprove(body)
			} else {
				t.Error("unexpected mutation")
			}
			json.NewEncoder(w).Encode(request)
		default:
			t.Errorf("unexpected relay path: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func delegationPOST(t *testing.T, s *Server, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	token := newCSRFToken()
	req := httptest.NewRequest(http.MethodPost, "https://portal.test"+path, bytes.NewReader(data))
	req.Header.Set("X-User", "alice@example.com")
	req.Header.Set("Origin", "https://portal.test")
	req.Header.Set(csrfHeaderName, token)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: token})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func approvalBody() map[string]any {
	return map[string]any{"request_id": testDelegationID, "namespace": "attacker", "minutes": 15,
		"devices": []string{"mac-id"}, "confirmed": true,
		"controller_fingerprint": delegationFixture().ControllerFingerprint,
		"device_fingerprints":    map[string]string{"mac-id": transport.Fingerprint([]byte("mac"))}}
}

func TestDelegationPagePreservesRequestThroughLogin(t *testing.T) {
	s := newOAuthPortal(t, resolveOKAs("alice", "user"))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/webfetch/approve?request="+testDelegationID, nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d", rec.Code)
	}
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("next"); got != "/webfetch/approve?request="+testDelegationID {
		t.Fatalf("login lost request: %q", got)
	}
}

func TestDelegationPageShowsOwnedDevicesAndEscapesUntrustedLabels(t *testing.T) {
	req := delegationFixture()
	req.Label = `<img src=x onerror="boom()">`
	s := delegationPortal(t, req, []delegationDevice{
		{Name: "owned-id", Owner: "alice", Alias: "My Mac", Fingerprint: transport.Fingerprint([]byte("owned"))},
		{Name: "shared-id", Owner: "bob", Shared: true, Fingerprint: transport.Fingerprint([]byte("shared"))},
		{Name: "no-identity", Owner: "alice"},
	}, nil)
	r := httptest.NewRequest(http.MethodGet, "/webfetch/approve?request="+testDelegationID, nil)
	r.Header.Set("X-User", "alice@example.com")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	page := rec.Body.String()
	for _, wanted := range []string{"owned-id", "My Mac", "&lt;img", `value="15" selected`, req.ControllerFingerprint, `id="delegateApprove" disabled`} {
		if !strings.Contains(page, wanted) {
			t.Errorf("page missing %q", wanted)
		}
	}
	for _, forbidden := range []string{"shared-id", "no-identity", "<img src=x", `name="device" checked`} {
		if strings.Contains(page, forbidden) {
			t.Errorf("page exposed %q", forbidden)
		}
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("cache control = %q", got)
	}
}

func TestDelegationApprovalBindsOwnerAndDisplayedIdentities(t *testing.T) {
	called := false
	s := delegationPortal(t, delegationFixture(), []delegationDevice{{Name: "mac-id", Owner: "alice", Fingerprint: transport.Fingerprint([]byte("mac"))}}, func(body map[string]any) {
		called = true
		if body["namespace"] != "alice" {
			t.Errorf("namespace not overwritten: %#v", body)
		}
		if body["controller_fingerprint"] != delegationFixture().ControllerFingerprint {
			t.Error("controller identity lost")
		}
		fps, ok := body["device_fingerprints"].(map[string]any)
		if !ok || fps["mac-id"] != transport.Fingerprint([]byte("mac")) {
			t.Error("device identity lost")
		}
	})
	rec := delegationPOST(t, s, "/api/delegations/approve", approvalBody())
	if rec.Code != http.StatusOK || !called {
		t.Fatalf("approval = %d called=%v: %s", rec.Code, called, rec.Body.String())
	}
}

func TestDelegationApprovalRejectsUnconfirmedForeignOrChangedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
		status int
	}{
		{"confirmation", func(b map[string]any) { b["confirmed"] = false }, http.StatusBadRequest},
		{"minutes", func(b map[string]any) { b["minutes"] = 61 }, http.StatusBadRequest},
		{"empty", func(b map[string]any) { b["devices"] = []string{} }, http.StatusBadRequest},
		{"foreign", func(b map[string]any) { b["devices"] = []string{"other-id"} }, http.StatusForbidden},
		{"shared", func(b map[string]any) { b["devices"] = []string{"shared-id"} }, http.StatusForbidden},
		{"duplicate", func(b map[string]any) { b["devices"] = []string{"mac-id", "mac-id"} }, http.StatusForbidden},
		{"controller changed", func(b map[string]any) { b["controller_fingerprint"] = "different" }, http.StatusConflict},
		{"device changed", func(b map[string]any) { b["device_fingerprints"] = map[string]string{"mac-id": "different"} }, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := delegationPortal(t, delegationFixture(), []delegationDevice{
				{Name: "mac-id", Owner: "alice", Fingerprint: transport.Fingerprint([]byte("mac"))},
				{Name: "shared-id", Owner: "bob", Shared: true, Fingerprint: transport.Fingerprint([]byte("shared"))},
			}, nil)
			body := approvalBody()
			tc.change(body)
			rec := delegationPOST(t, s, "/api/delegations/approve", body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
		})
	}
}

func TestDelegationDecisionsRequirePOSTAndCSRF(t *testing.T) {
	s := delegationPortal(t, delegationFixture(), nil, nil)
	for _, path := range []string{"/api/delegations/approve", "/api/delegations/reject"} {
		for _, tc := range []struct {
			method, origin string
			status         int
		}{
			{http.MethodGet, "", http.StatusMethodNotAllowed},
			{http.MethodPost, "https://portal.test", http.StatusForbidden},
			{http.MethodPost, "https://evil.test", http.StatusForbidden},
		} {
			req := httptest.NewRequest(tc.method, "https://portal.test"+path, strings.NewReader(`{}`))
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("X-User", "alice@example.com")
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Errorf("%s %s origin=%s: %d want %d", tc.method, path, tc.origin, rec.Code, tc.status)
			}
		}
	}
}

func TestDelegationRequestCannotReadAnotherOwnersGrant(t *testing.T) {
	req := delegationFixture()
	req.Status = "approved"
	req.Namespace = "bob"
	s := delegationPortal(t, req, nil, nil)
	for _, path := range []string{"/api/delegations/request?id=" + testDelegationID, "/webfetch/approve?request=" + testDelegationID} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-User", "alice@example.com")
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("foreign grant status = %d", rec.Code)
		}
		if strings.HasPrefix(path, "/webfetch/") {
			page := rec.Body.String()
			if !strings.Contains(page, "already belong to another account") || !strings.Contains(page, "cached page") || !strings.Contains(page, `id="out"`) {
				t.Fatal("owner cannot recover from a foreign/cached approval link")
			}
			for _, secret := range []string{"bob", req.Label, req.ControllerFingerprint, testDelegationID} {
				if strings.Contains(page, secret) {
					t.Errorf("foreign grant detail exposed: %q", secret)
				}
			}
		}
	}
}

func TestDelegationPageExplainsRelayDenialWithoutForwardingItsBody(t *testing.T) {
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "alice", "role": "user"})
		case "/admin/delegations/request":
			http.Error(w, "delegation forbidden: private upstream detail", http.StatusForbidden)
		default:
			t.Errorf("unexpected relay call: %s", r.URL.Path)
		}
	})
	r := httptest.NewRequest(http.MethodGet, "/webfetch/approve?request="+testDelegationID, nil)
	r.Header.Set("X-User", "alice@example.com")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Header().Get("Content-Type"), "text/html") || !strings.Contains(rec.Body.String(), "fresh request") || strings.Contains(rec.Body.String(), "private upstream detail") {
		t.Fatalf("unhelpful or leaking denial: %d %s", rec.Code, rec.Body.String())
	}
}

func TestDelegationRejectOverwritesNamespace(t *testing.T) {
	called := false
	s := delegationPortal(t, delegationFixture(), nil, func(body map[string]any) {
		called = true
		if body["namespace"] != "alice" || body["request_id"] != testDelegationID {
			t.Errorf("reject body = %#v", body)
		}
	})
	rec := delegationPOST(t, s, "/api/delegations/reject", map[string]any{"request_id": testDelegationID, "namespace": "attacker"})
	if rec.Code != http.StatusOK || !called {
		t.Fatalf("reject = %d called=%v", rec.Code, called)
	}
}

func TestDelegationInactivePageCannotApproveAgain(t *testing.T) {
	for _, status := range []string{"approved", "rejected", "expired"} {
		t.Run(status, func(t *testing.T) {
			req := delegationFixture()
			req.Status, req.Namespace = status, "alice"
			expiry := time.Now().Add(time.Minute)
			req.ExpiresAt = &expiry
			s := delegationPortal(t, req, nil, nil)
			r := httptest.NewRequest(http.MethodGet, "/webfetch/approve?request="+testDelegationID, nil)
			r.Header.Set("X-User", "alice@example.com")
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, r)
			if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `id="delegateApprove"`) {
				t.Fatalf("inactive page = %d: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `data-status="`+status+`"`) {
				t.Error("missing request status")
			}
		})
	}
}

func TestDelegationRequestRequiresAuthentication(t *testing.T) {
	s := delegationPortal(t, delegationFixture(), nil, nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/delegations/request?id="+testDelegationID, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d want 401", rec.Code)
	}
}
