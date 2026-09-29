package portal

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

type sentMail struct{ to, subject, body string }
type fakeMailSender struct{ messages chan sentMail }

func (f *fakeMailSender) Send(to, subject, body string) error {
	f.messages <- sentMail{to, subject, body}
	return nil
}
func newFakeMailSender() *fakeMailSender { return &fakeMailSender{make(chan sentMail, 10)} }

func TestMailConfiguration(t *testing.T) {
	for missing := -1; missing < 4; missing++ {
		values := []string{"smtp.example:587", "user", "password", "wanctl <wanctl@example.com>"}
		if missing >= 0 {
			values[missing] = ""
		}
		s := New(Config{SMTPAddr: values[0], SMTPUser: values[1], SMTPPassword: values[2], MailFrom: values[3]})
		if s.mailEnabled() != (missing == -1) {
			t.Fatalf("missing %d: mail enabled = %v", missing, s.mailEnabled())
		}
	}
}

func TestOAuthEmailAndPages(t *testing.T) {
	// Email fixtures follow GitHub's documented /user/emails response schema.
	for _, tc := range []struct {
		name, response, want string
		on                   bool
		status               int
	}{
		{"off", `[{"email":"primary@example.com","primary":true,"verified":true}]`, "", false, 200},
		{"primary verified", `[{"email":"other@example.com","primary":false,"verified":true},{"email":"unverified@example.com","primary":true,"verified":false},{"email":"8437@users.noreply.github.com","primary":true,"verified":true},{"email":"primary@example.com","primary":true,"verified":true}]`, "primary@example.com", true, 200},
		{"noreply", `[{"email":"8437@USERS.NOREPLY.GITHUB.COM","primary":true,"verified":true}]`, "", true, 200},
		{"no primary", `[{"email":"other@example.com","primary":false,"verified":true}]`, "", true, 200},
		{"unverified", `[{"email":"other@example.com","primary":true,"verified":false}]`, "", true, 200},
		{"empty", `[]`, "", true, 200},
		{"upstream failure", `{"message":"Bad credentials"}`, "", true, 401},
		{"malformed", `{`, "", true, 200},
		{"transport failure", ``, "", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			s := accessPortal(t, func(b map[string]string, w http.ResponseWriter) {
				if b["email"] != tc.want {
					t.Errorf("resolved email = %q, want %q", b["email"], tc.want)
				}
				resolvePendingInvite(b, w)
			}, &calls)
			if tc.on {
				s.mail = newFakeMailSender()
			}
			emailCalls := 0
			inner := s.hc.Transport
			// A separate GitHub transport also verifies proxy routing for /user/emails.
			s.ghc = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/user/emails" {
					return inner.RoundTrip(r)
				}
				emailCalls++
				if r.Header.Get("Authorization") != "Bearer gho_test" {
					t.Error("email request lost user token")
				}
				if tc.status == 0 {
					return nil, fmt.Errorf("unreachable")
				}
				w := httptest.NewRecorder()
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.response)
				return w.Result(), nil
			})}
			rr := httptest.NewRecorder()
			s.handleAuthStart(rr, httptest.NewRequest("GET", "/auth/github", nil))
			loc, _ := url.Parse(rr.Header().Get("Location"))
			if tc.on && loc.Query().Get("scope") != "user:email" {
				t.Fatal("missing scope")
			}
			if !tc.on && loc.Query().Has("scope") {
				t.Fatal("unexpected scope")
			}
			rr = httptest.NewRecorder()
			s.handleAuthLogin(rr, httptest.NewRequest("GET", "/auth/login", nil))
			old := "No GitHub permissions are requested — only your public profile."
			current := "Only your public profile and your email address, to tell you about your account."
			if strings.Contains(rr.Body.String(), old) == tc.on || strings.Contains(rr.Body.String(), current) != tc.on {
				t.Fatal("wrong permission copy")
			}
			cb := loginThroughCallback(t, s, "")
			if cb.Code != http.StatusSeeOther {
				t.Fatalf("callback = %d %s", cb.Code, cb.Body.String())
			}
			cookie := sessionCookie(t, cb)
			p, err := s.decodeSession(cookie.Value)
			if err != nil || p.Email != tc.want {
				t.Fatalf("principal = %#v, %v", p, err)
			}
			wantCalls := 0
			if tc.on {
				wantCalls = 1
			}
			if emailCalls != wantCalls {
				t.Fatalf("email calls = %d", emailCalls)
			}
			rr = httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/pending", nil)
			req.AddCookie(cookie)
			s.handlePending(rr, req)
			if strings.Contains(rr.Body.String(), `type="email"`) != (tc.on && tc.want == "") {
				t.Fatal("wrong email field visibility")
			}
		})
	}
}

func TestApprovalMailDecisions(t *testing.T) {
	for _, tc := range []struct {
		name, decision, email, origin string
		status                        int
		on, send                      bool
	}{
		{"approved", "approved", "applicant@example.com", "", 200, true, true},
		{"normalized decision", " APPROVED ", "applicant@example.com", "", 200, true, true},
		{"canonical origin", "approved", "applicant@example.com", "https://canonical.example", 200, true, true},
		{"declined", "declined", "applicant@example.com", "", 200, true, false},
		{"no email", "approved", "", "", 200, true, false},
		{"mail off", "approved", "applicant@example.com", "", 200, false, false},
		{"already decided", "approved", "applicant@example.com", "", 404, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			s := accessPortal(t, resolveOKAs("admin", "admin"), &calls)
			fake := newFakeMailSender()
			if tc.on {
				s.mail = fake
			}
			s.publicOrigin = tc.origin
			inner := s.hc.Transport
			s.hc.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/admin/access-requests/decide" {
					return inner.RoundTrip(r)
				}
				rr := httptest.NewRecorder()
				rr.WriteHeader(tc.status)
				json.NewEncoder(rr).Encode(map[string]any{"id": 7, "login": "octocat", "email": tc.email, "status": strings.ToLower(strings.TrimSpace(tc.decision))})
				return rr.Result(), nil
			})
			h := s.Handler()
			cookies := inviteSession(t, s, h)
			// Direct handler invocation avoids CSRF origin coupling to the canonical origin.
			req := httptest.NewRequest("POST", "https://portal.test/api/access-requests/decide", strings.NewReader(`{"id":7,"decision":"`+tc.decision+`"}`))
			for _, c := range cookies {
				req.AddCookie(c)
			}
			rr := httptest.NewRecorder()
			s.handleAccessDecide(rr, req)
			if rr.Code != tc.status {
				t.Fatalf("status = %d", rr.Code)
			}
			if tc.send {
				select {
				case message := <-fake.messages:
					origin := tc.origin
					if origin == "" {
						origin = "https://portal.test"
					}
					if message.to != tc.email || message.subject != approvalMailSubject || !strings.Contains(message.body, origin+"/") || !strings.Contains(message.body, "octocat") {
						t.Fatalf("message = %#v", message)
					}
				case <-time.After(time.Second):
					t.Fatal("no approval email")
				}
			}
			select {
			case message := <-fake.messages:
				t.Fatalf("unexpected extra message: %#v", message)
			case <-time.After(20 * time.Millisecond):
			}
		})
	}
}
