package portal

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wanctl/internal/relay"
)

// tokenIssueStore replaces only persistence: the portal's routing, CSRF check,
// principal resolution and forwarding, and the relay's admin handlers all run.
type tokenIssueStore struct {
	relay.AdminStore
	issued []string
}

func (s *tokenIssueStore) ResolveIdentity(provider, subject, login, name, invite, reserved, email string) (string, string, error) {
	return "attacker", "user", nil
}

func (s *tokenIssueStore) IssueToken(ns, label string, days int) (string, error) {
	s.issued = append(s.issued, ns)
	return "synthetic-test-token", nil
}

func postAsSignedInUser(t *testing.T, store *tokenIssueStore, path, body string) int {
	t.Helper()
	r := relay.New(relay.EnvTokenStore("test:attacker"))
	r.SetAdmin(store)
	r.SetAdminSecret("synthetic-admin-secret-0123456789abcdef")
	r.SetPortalNS("portal")
	s := New(Config{RelayAdminURL: "https://relay.test", AdminSecret: "synthetic-admin-secret-0123456789abcdef"})
	s.ghClientID = "synthetic-github-client"
	s.sessionKey = []byte(strings.Repeat("s", 32))
	s.publicOrigin = "https://portal.test"
	s.hc = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		r.Handler().ServeHTTP(rec, req)
		return rec.Result(), nil
	})}
	cookie, err := s.encodeSession(&principal{Provider: "github", Subject: "100", Login: "attacker", Expires: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "https://portal.test"+path, strings.NewReader(body))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie})
	csrf := newCSRFToken()
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: csrf})
	req.Header.Set(csrfHeaderName, csrf)
	req.Header.Set("Origin", "https://portal.test")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code
}

// A user's JSON reaches the relay next to the namespace the portal resolved.
// No key the user sends may take that namespace's place.
func TestProxyPostKeepsTheCallersNamespace(t *testing.T) {
	for _, body := range []string{
		`{"nameſpace":"victim","label":"x"}`, // U+017F folds to s in encoding/json
		`{"nameſpace":"portal","label":"x"}`,
		`{"Namespace":"victim","label":"x"}`,
		`{"namespace":"victim","label":"x"}`,
	} {
		store := &tokenIssueStore{}
		code := postAsSignedInUser(t, store, "/api/tokens", body)
		if code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, code)
		}
		if len(store.issued) != 0 {
			t.Errorf("%s: a token was issued for %v", body, store.issued)
		}
	}
}

func TestProxyPostForwardsTheFieldsThePortalSends(t *testing.T) {
	store := &tokenIssueStore{}
	if code := postAsSignedInUser(t, store, "/api/tokens", `{"label":"laptop","days":30}`); code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	if len(store.issued) != 1 || store.issued[0] != "attacker" {
		t.Fatalf("issued = %v, want [attacker]", store.issued)
	}
}

func TestAccessEmailCannotOverrideSessionFields(t *testing.T) {
	for _, trustedEmail := range []string{"", "github@example.com"} {
		var calls []string
		s := accessPortal(t, resolvePendingInvite, &calls)
		s.mail = newFakeMailSender()
		cookie, err := s.encodeSession(&principal{Provider: "github", Subject: "100", Login: "attacker", Email: trustedEmail, Expires: time.Now().Add(time.Hour).Unix()})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", "https://portal.test/auth/request-access", strings.NewReader(`{"email":" typed@example.com ","provider":"header","Provider":"header","subject":"1","ſubject":"2","login":"victim","Login":"admin","namespace":"portal","nameſpace":"portal"}`))
		req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie})
		rr := httptest.NewRecorder()
		s.handleAccessRequest(rr, req)
		if rr.Code != 200 {
			t.Fatalf("response = %d %s", rr.Code, rr.Body.String())
		}
		wantEmail := trustedEmail
		if wantEmail == "" {
			wantEmail = "typed@example.com"
		}
		want := `POST /admin/access-requests {"email":"` + wantEmail + `","login":"attacker","note":"","provider":"github","subject":"100"}`
		if len(calls) != 1 || calls[0] != want {
			t.Fatalf("calls = %v", calls)
		}
	}
}
