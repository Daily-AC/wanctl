package portal

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// doorRelay is the relay's side of the email door, in memory: one identity's
// contact address, its live link, and every call the portal made.
type doorRelay struct {
	resolve   string // "ok" or "pending"
	confirmed string // confirmed address, "" for none
	legacy    string // unconfirmed carried-over address
	pending   string // address of the live link
	sendCode  int    // non-zero: refuse the send with this status
	sendBody  string
	retry     string
	peekState string
	calls     []string
}

func (d *doorRelay) serve(req *http.Request, rr *httptest.ResponseRecorder) bool {
	if req.URL.Host != "relay.test" {
		return false
	}
	var raw []byte
	if req.Body != nil {
		raw, _ = io.ReadAll(req.Body)
	}
	body := map[string]any{}
	json.Unmarshal(raw, &body)
	d.calls = append(d.calls, req.URL.Path+" "+string(raw))
	switch req.URL.Path {
	case "/admin/resolve-user":
		if d.resolve == "pending" {
			rr.WriteHeader(http.StatusForbidden)
			io.WriteString(rr, pendingInviteBody)
			return true
		}
		json.NewEncoder(rr).Encode(map[string]any{"namespace": "octocat", "role": "admin", "email_confirmed": d.confirmed != ""})
	case "/admin/contact-email":
		if req.URL.Query().Get("provider") != "github" || req.URL.Query().Get("subject") != "8437" {
			rr.WriteHeader(http.StatusBadRequest)
			return true
		}
		out := map[string]any{"address": d.legacy, "confirmed_at": nil, "pending": d.pending}
		if d.confirmed != "" {
			out["address"], out["confirmed_at"] = d.confirmed, "2026-09-30T00:00:00Z"
		}
		json.NewEncoder(rr).Encode(out)
	case "/admin/contact-email/send":
		if d.sendCode != 0 {
			if d.retry != "" {
				rr.Header().Set("Retry-After", d.retry)
			}
			rr.WriteHeader(d.sendCode)
			io.WriteString(rr, d.sendBody)
			return true
		}
		d.pending, _ = body["address"].(string)
		json.NewEncoder(rr).Encode(map[string]any{"id": 5, "token": "tok+/=", "address": d.pending, "expires_at": "2026-10-01T00:00:00Z"})
	case "/admin/contact-email/failed":
		d.pending = ""
		rr.WriteHeader(http.StatusNoContent)
	case "/admin/contact-email/sent":
		rr.WriteHeader(http.StatusNoContent)
	case "/admin/contact-email/cancel":
		if body["provider"] != "github" || body["subject"] != "8437" {
			rr.WriteHeader(http.StatusBadRequest)
			return true
		}
		d.pending = ""
		rr.WriteHeader(http.StatusNoContent)
	case "/admin/contact-email/peek":
		if d.peekState == "" {
			rr.WriteHeader(http.StatusNotFound)
			io.WriteString(rr, "token-unknown")
			return true
		}
		json.NewEncoder(rr).Encode(map[string]any{"state": d.peekState, "login": "octocat", "address": "person@example.com"})
	case "/admin/contact-email/confirm":
		if d.peekState != "live" {
			rr.WriteHeader(http.StatusGone)
			io.WriteString(rr, "token-"+d.peekState)
			return true
		}
		d.confirmed, d.pending, d.peekState = "person@example.com", "", "used"
		json.NewEncoder(rr).Encode(map[string]any{"provider": "github", "subject": "8437", "login": "octocat", "address": "person@example.com", "next": "/enroll"})
	case "/admin/access-requests/status":
		json.NewEncoder(rr).Encode(map[string]any{"status": "none", "can_apply": true})
	case "/admin/access-requests":
		json.NewEncoder(rr).Encode(map[string]any{"id": 1, "status": "pending"})
	default:
		rr.WriteHeader(http.StatusNotFound)
	}
	return true
}

type failingMail struct{ tried int }

func (f *failingMail) Send(string, mailContent) error {
	f.tried++
	return errors.New("SMTP 554 refused for person@example.com")
}

// doorPortal is an OAuth portal with mail on — the configuration the door
// exists in — and the fake relay above.
func doorPortal(t *testing.T, d *doorRelay) (*Server, *fakeMailSender) {
	t.Helper()
	s := newOAuthPortal(t, nil)
	inner := s.hc.Transport
	s.hc = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		rr := httptest.NewRecorder()
		if d.serve(req, rr) {
			return rr.Result(), nil
		}
		return inner.RoundTrip(req)
	})}
	mail := newFakeMailSender()
	s.mail = mail
	return s, mail
}

func signedIn(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	return sessionCookie(t, loginThroughCallback(t, s, ""))
}

func get(h http.Handler, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// post sends a same-origin POST with a valid double-submit CSRF pair.
func post(h http.Handler, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "http://portal.test"+path, strings.NewReader(body))
	req.Host = "portal.test"
	req.Header.Set("Origin", "http://portal.test")
	csrf := newCSRFToken()
	req.Header.Set(csrfHeaderName, csrf)
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: csrf})
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// Sign-in asks GitHub for nothing. Addresses are typed and confirmed, so the
// user:email permission and the /user/emails call are gone whether or not
// mail is configured; the sign-in page says so, and with mail on it also says
// an email comes next.
func TestOAuthAsksGitHubForNoEmail(t *testing.T) {
	for _, mailOn := range []bool{false, true} {
		d := &doorRelay{resolve: "ok", confirmed: "person@example.com"}
		s, _ := doorPortal(t, d)
		if !mailOn {
			s.mail = nil
		}
		emailCalls := 0
		inner := s.hc.Transport
		s.ghc = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/user/emails" {
				emailCalls++
			}
			return inner.RoundTrip(r)
		})}
		rr := httptest.NewRecorder()
		s.handleAuthStart(rr, httptest.NewRequest("GET", "/auth/github", nil))
		loc, _ := url.Parse(rr.Header().Get("Location"))
		if loc.Query().Has("scope") {
			t.Fatalf("mail %v: scope requested: %s", mailOn, loc)
		}
		rr = httptest.NewRecorder()
		s.handleAuthLogin(rr, httptest.NewRequest("GET", "/auth/login", nil))
		page := rr.Body.String()
		if !strings.Contains(page, "No GitHub permissions are requested") || strings.Contains(page, "your email address, to tell you") {
			t.Fatalf("mail %v: permission copy wrong", mailOn)
		}
		if strings.Contains(page, "confirm an email address") != mailOn {
			t.Fatalf("mail %v: next-step copy wrong", mailOn)
		}
		cookie := signedIn(t, s)
		p, err := s.decodeSession(cookie.Value)
		if err != nil || p.Login != "octocat" {
			t.Fatalf("principal = %#v, %v", p, err)
		}
		if emailCalls != 0 {
			t.Fatalf("mail %v: /user/emails called %d times", mailOn, emailCalls)
		}
		for _, call := range d.calls {
			if strings.HasPrefix(call, "/admin/resolve-user") && strings.Contains(call, "email") {
				t.Fatalf("resolve still carries an email: %s", call)
			}
		}
	}
}

// Without a confirmed address an admitted user gets no further than the door,
// on every way in: the callback, page loads, and the API the SPA calls.
func TestDoorStopsAdmittedUserWithoutConfirmedEmail(t *testing.T) {
	d := &doorRelay{resolve: "ok", legacy: "old@example.com"}
	s, _ := doorPortal(t, d)
	h := s.Handler()

	cb := loginThroughCallback(t, s, "/enroll")
	if cb.Code != http.StatusSeeOther || cb.Header().Get("Location") != "/auth/email?next=%2Fenroll" {
		t.Fatalf("callback = %d %q", cb.Code, cb.Header().Get("Location"))
	}
	cookie := sessionCookie(t, cb)
	for path, want := range map[string]string{
		"/":                          "/auth/email?next=%2F",
		"/enroll":                    "/auth/email?next=%2Fenroll",
		"/pending":                   "/",
		"/oauth/authorize?client_id": "",
	} {
		rr := get(h, path, cookie)
		if want == "" {
			continue
		}
		if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != want {
			t.Fatalf("GET %s = %d %q, want %q", path, rr.Code, rr.Header().Get("Location"), want)
		}
	}
	for _, path := range []string{"/api/me", "/api/devices", "/api/tokens", "/api/access-requests", "/api/invites"} {
		rr := get(h, path, cookie)
		if rr.Code != http.StatusForbidden || strings.TrimSpace(rr.Body.String()) != emailRequiredBody {
			t.Fatalf("GET %s = %d %q", path, rr.Code, rr.Body.String())
		}
	}
	// Mutations are refused the same way, before any relay write.
	before := len(d.calls)
	rr := post(h, "/api/tokens", `{"label":"x"}`, cookie)
	if rr.Code != http.StatusForbidden || strings.TrimSpace(rr.Body.String()) != emailRequiredBody {
		t.Fatalf("POST /api/tokens = %d %q", rr.Code, rr.Body.String())
	}
	for _, call := range d.calls[before:] {
		if !strings.HasPrefix(call, "/admin/resolve-user") {
			t.Fatalf("relay reached past the door: %s", call)
		}
	}

	// The door page offers the carried-over address to confirm.
	rr = get(h, "/auth/email?next=%2Fenroll", cookie)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `data-req="form"`) || !strings.Contains(rr.Body.String(), `value="old@example.com"`) {
		t.Fatalf("door = %d", rr.Code)
	}

	// Once confirmed, the same session walks straight through.
	d.confirmed = "person@example.com"
	if rr := get(h, "/", cookie); rr.Code != 200 {
		t.Fatalf("GET / after confirming = %d %q", rr.Code, rr.Header().Get("Location"))
	}
	if rr := get(h, "/auth/email?next=%2Fenroll", cookie); rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/enroll" {
		t.Fatalf("door after confirming = %d %q", rr.Code, rr.Header().Get("Location"))
	}
	var me map[string]any
	rr = get(h, "/api/me", cookie)
	json.Unmarshal(rr.Body.Bytes(), &me)
	if rr.Code != 200 || me["mail"] != true {
		t.Fatalf("/api/me = %d %s", rr.Code, rr.Body.String())
	}
}

// An applicant passes the same door, and the request form is refused on the
// server side, not only by the page's redirect.
func TestDoorStopsApplicantWithoutConfirmedEmail(t *testing.T) {
	d := &doorRelay{resolve: "pending"}
	s, _ := doorPortal(t, d)
	h := s.Handler()
	cb := loginThroughCallback(t, s, "")
	if cb.Header().Get("Location") != "/pending" {
		t.Fatalf("callback went to %q", cb.Header().Get("Location"))
	}
	cookie := sessionCookie(t, cb)
	rr := get(h, "/pending", cookie)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/auth/email?next=%2Fpending" {
		t.Fatalf("GET /pending = %d %q", rr.Code, rr.Header().Get("Location"))
	}
	rr = post(h, "/auth/request-access", `{"note":"hi","email":"typed@example.com"}`, cookie)
	if rr.Code != http.StatusForbidden || strings.TrimSpace(rr.Body.String()) != emailRequiredBody {
		t.Fatalf("request-access = %d %q", rr.Code, rr.Body.String())
	}
	for _, call := range d.calls {
		if strings.HasPrefix(call, "/admin/access-requests ") {
			t.Fatalf("application filed without an address: %s", call)
		}
	}

	d.confirmed = "person@example.com"
	rr = get(h, "/pending", cookie)
	if rr.Code != 200 || strings.Contains(rr.Body.String(), `type="email"`) {
		t.Fatalf("pending page = %d; email field present: %v", rr.Code, strings.Contains(rr.Body.String(), `type="email"`))
	}
	rr = post(h, "/auth/request-access", `{"note":"hi","email":"typed@example.com"}`, cookie)
	if rr.Code != 200 {
		t.Fatalf("request-access after confirming = %d %q", rr.Code, rr.Body.String())
	}
	last := d.calls[len(d.calls)-1]
	if !strings.HasPrefix(last, "/admin/access-requests ") || strings.Contains(last, "email") || strings.Contains(last, "typed@") {
		t.Fatalf("filed = %s", last)
	}
}

func TestDoorSendsTheLink(t *testing.T) {
	d := &doorRelay{resolve: "ok"}
	s, mail := doorPortal(t, d)
	h := s.Handler()
	cookie := signedIn(t, s)

	rr := post(h, "/auth/email/send", `{"address":"person@example.com","next":"//evil.example"}`, cookie)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"address":"person@example.com"`) {
		t.Fatalf("send = %d %s", rr.Code, rr.Body.String())
	}
	var sendCall string
	for _, call := range d.calls {
		if strings.HasPrefix(call, "/admin/contact-email/send ") {
			sendCall = call
		}
	}
	for _, want := range []string{`"provider":"github"`, `"subject":"8437"`, `"login":"octocat"`, `"next":"/"`} {
		if !strings.Contains(sendCall, want) {
			t.Fatalf("send call %s lacks %s", sendCall, want)
		}
	}
	if last := d.calls[len(d.calls)-1]; last != `/admin/contact-email/sent {"id":5}` {
		t.Fatalf("after a delivered mail the relay heard %s", last)
	}
	message := <-mail.messages
	link := "http://portal.test/auth/email/confirm?t=tok%2B%2F%3D"
	if message.to != "person@example.com" || message.subject != confirmMailSubject {
		t.Fatalf("mail = %#v", message)
	}
	for _, part := range []string{message.text, message.html} {
		for _, want := range []string{link, "octocat", "person@example.com", "portal.test", "24"} {
			if !strings.Contains(part, want) && !strings.Contains(part, strings.ReplaceAll(want, "&", "&amp;")) {
				t.Fatalf("mail part lacks %q:\n%s", want, part)
			}
		}
	}
	// The door now shows where the link went.
	rr = get(h, "/auth/email", cookie)
	if !strings.Contains(rr.Body.String(), `data-req="sent"`) || !strings.Contains(rr.Body.String(), "person@example.com") {
		t.Fatalf("door after send:\n%s", rr.Body.String())
	}

	// Limits come back as JSON with the wait, for the page to put in words.
	d.sendCode, d.sendBody, d.retry = http.StatusTooManyRequests, "rate-address", "540"
	rr = post(h, "/auth/email/send", `{"address":"person@example.com"}`, cookie)
	var refused map[string]any
	json.Unmarshal(rr.Body.Bytes(), &refused)
	if rr.Code != http.StatusTooManyRequests || refused["error"] != "rate-address" || refused["retry_after"] != float64(540) {
		t.Fatalf("rate = %d %s", rr.Code, rr.Body.String())
	}

	// A mail server that refuses is reported on the spot, and the relay is
	// told, so the dead link neither supersedes the live one nor holds the
	// mailbox's interval.
	d.sendCode, d.retry = 0, ""
	failing := &failingMail{}
	s.mail = failing
	rr = post(h, "/auth/email/send", `{"address":"other@example.com"}`, cookie)
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "mail-failed") || failing.tried != 1 {
		t.Fatalf("mail failure = %d %s", rr.Code, rr.Body.String())
	}
	if last := d.calls[len(d.calls)-1]; last != `/admin/contact-email/failed {"id":5}` {
		t.Fatalf("last relay call = %s", last)
	}

	// Signed out, nothing is sent.
	if rr := post(h, "/auth/email/send", `{"address":"person@example.com"}`); rr.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous send = %d", rr.Code)
	}

	// Cancelling a change reaches the relay for the session's identity only,
	// and only with the CSRF pair.
	d.pending = "person@example.com"
	req := httptest.NewRequest("POST", "/auth/email/cancel", strings.NewReader(`{"subject":"1"}`))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || d.pending == "" {
		t.Fatalf("cancel without CSRF = %d", rec.Code)
	}
	if rr := post(h, "/auth/email/cancel", `{"subject":"1"}`, cookie); rr.Code != http.StatusNoContent || d.pending != "" {
		t.Fatalf("cancel = %d %s", rr.Code, rr.Body.String())
	}
}

// Opening the link changes nothing; only the page's POST confirms, and only
// with the CSRF pair. The browser that sent the link goes on to where it was
// headed; any other browser is told it is done.
func TestConfirmLinkOnlyConfirmsOnPost(t *testing.T) {
	d := &doorRelay{resolve: "ok", pending: "person@example.com", peekState: "live"}
	s, _ := doorPortal(t, d)
	h := s.Handler()

	for range 3 {
		rr := get(h, "/auth/email/confirm?t=tok")
		if rr.Code != 200 || !strings.Contains(rr.Body.String(), `data-req="live"`) || !strings.Contains(rr.Body.String(), `data-token="tok"`) {
			t.Fatalf("GET confirm = %d", rr.Code)
		}
	}
	for _, call := range d.calls {
		if strings.HasPrefix(call, "/admin/contact-email/confirm") {
			t.Fatalf("GET reached confirm: %v", d.calls)
		}
	}
	if d.confirmed != "" {
		t.Fatal("GET confirmed the address")
	}

	// No CSRF pair, no confirmation.
	req := httptest.NewRequest("POST", "/auth/email/confirm", strings.NewReader(`{"token":"tok"}`))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden || d.confirmed != "" {
		t.Fatalf("POST without CSRF = %d", rr.Code)
	}

	// A phone that never signed in: done, no onward link.
	rr = post(h, "/auth/email/confirm", `{"token":"tok"}`)
	if rr.Code != 200 || strings.TrimSpace(rr.Body.String()) != `{"next":""}` || d.confirmed != "person@example.com" {
		t.Fatalf("anonymous confirm = %d %s", rr.Code, rr.Body.String())
	}
	// Reuse is refused, and the page now says so.
	rr = post(h, "/auth/email/confirm", `{"token":"tok"}`)
	if rr.Code != http.StatusGone || !strings.Contains(rr.Body.String(), "token-used") {
		t.Fatalf("reuse = %d %s", rr.Code, rr.Body.String())
	}
	if rr := get(h, "/auth/email/confirm?t=tok"); !strings.Contains(rr.Body.String(), `data-req="used"`) {
		t.Fatal("used link page")
	}
	d.peekState = "expired"
	if rr := get(h, "/auth/email/confirm?t=tok"); !strings.Contains(rr.Body.String(), `data-req="expired"`) {
		t.Fatal("expired link page")
	}
	rr = post(h, "/auth/email/confirm", `{"token":"tok"}`)
	if rr.Code != http.StatusGone || !strings.Contains(rr.Body.String(), "token-expired") {
		t.Fatalf("expired = %d %s", rr.Code, rr.Body.String())
	}
	d.peekState = ""
	if rr := get(h, "/auth/email/confirm?t=nope"); !strings.Contains(rr.Body.String(), `data-req="unknown"`) {
		t.Fatal("unknown link page")
	}
	if rr := get(h, "/auth/email/confirm"); !strings.Contains(rr.Body.String(), `data-req="unknown"`) {
		t.Fatal("tokenless link page")
	}

	// The same identity's own browser goes on to where it was headed.
	d.confirmed, d.peekState = "", "live"
	cookie := signedIn(t, s)
	rr = post(h, "/auth/email/confirm", `{"token":"tok"}`, cookie)
	if rr.Code != 200 || strings.TrimSpace(rr.Body.String()) != `{"next":"/enroll"}` {
		t.Fatalf("own confirm = %d %s", rr.Code, rr.Body.String())
	}
}

// Where the door cannot be passed it is not up: no mail means no links, and
// header mode has no identities of its own.
func TestNoDoorWithoutMail(t *testing.T) {
	d := &doorRelay{resolve: "ok"}
	s, _ := doorPortal(t, d)
	s.mail = nil
	h := s.Handler()
	cookie := signedIn(t, s)
	if rr := get(h, "/", cookie); rr.Code != 200 {
		t.Fatalf("GET / = %d %q", rr.Code, rr.Header().Get("Location"))
	}
	if rr := get(h, "/api/me", cookie); rr.Code != 200 || strings.Contains(rr.Body.String(), `"mail":true`) {
		t.Fatalf("/api/me = %d %s", rr.Code, rr.Body.String())
	}
	for _, path := range []string{"/auth/email", "/auth/email/status"} {
		if rr := get(h, path, cookie); rr.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d", path, rr.Code)
		}
	}
	for _, call := range d.calls {
		if strings.HasPrefix(call, "/admin/contact-email") {
			t.Fatalf("contact lookup with the door down: %s", call)
		}
	}
}
