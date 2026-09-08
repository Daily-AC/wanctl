package portal

import (
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const mobileNonce = "164b0e76-18ad-4aeb-b47c-11960e5166a7"

func TestMobileEnrollmentPreservesStateThroughOAuth(t *testing.T) {
	s := newOAuthPortal(t, resolveOKAs("alice", "admin"))
	next := "/enroll?mobile_state=" + mobileNonce
	rec := loginThroughCallback(t, s, next)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != next {
		t.Fatalf("callback lost mobile state: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	// Entering via /enroll (SSO/expired session) must also preserve the destination.
	rec = httptest.NewRecorder()
	s.handleEnroll(rec, httptest.NewRequest("GET", next, nil))
	location, _ := url.Parse(rec.Header().Get("Location"))
	if location.Query().Get("next") != next {
		t.Fatalf("login redirect lost state: %s", location)
	}
}

func TestMobileEnrollmentReturnLink(t *testing.T) {
	mintCalls := 0
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "alice"})
		case "/admin/enroll/mint":
			mintCalls++
			json.NewEncoder(w).Encode(map[string]any{"code": "ABCD-2345", "expires_in": 300})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	rec := httptest.NewRecorder()
	s.handleEnroll(rec, userReq("GET", "/enroll?mobile_state="+mobileNonce, nil))
	body := html.UnescapeString(rec.Body.String())
	want := string(mobileReturnURL(mobileNonce, "ABCD-2345"))
	if rec.Code != 200 || !strings.Contains(body, `href="`+want+`"`) || strings.Contains(body, "#ZgotmplZ") {
		t.Fatalf("missing app return link: %d %s", rec.Code, body)
	}
	if strings.Contains(body, "Paste it back into the terminal") {
		t.Fatal("mobile page still sends the user to a terminal")
	}
	for _, bad := range []string{"https://evil.example", "x", mobileNonce + "\"", strings.Repeat("a", 36)} {
		rec = httptest.NewRecorder()
		s.handleEnroll(rec, userReq("GET", "/enroll?mobile_state="+url.QueryEscape(bad), nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("invalid state accepted: %q", bad)
		}
	}
	if mintCalls != 1 {
		t.Fatalf("invalid request minted a credential: %d", mintCalls)
	}
}

func TestMobilePendingKeepsEnrollmentDestination(t *testing.T) {
	s := newOAuthPortal(t, func(body map[string]string, w http.ResponseWriter) {
		http.Error(w, pendingInviteBody, http.StatusForbidden)
	})
	next := "/enroll?mobile_state=" + mobileNonce
	rec := loginThroughCallback(t, s, next)
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || location.Path != "/pending" || location.Query().Get("next") != next {
		t.Fatalf("pending lost enrollment: %s", location)
	}
}
