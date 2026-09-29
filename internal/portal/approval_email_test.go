package portal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type sentMail struct {
	to string
	mailContent
}
type fakeMailSender struct{ messages chan sentMail }

func (f *fakeMailSender) Send(to string, m mailContent) error {
	f.messages <- sentMail{to, m}
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
					if message.to != tc.email || message.subject != approvalMailSubject {
						t.Fatalf("message = %#v", message)
					}
					for _, part := range []string{message.text, message.html} {
						if !strings.Contains(part, origin+"/") || !strings.Contains(part, "octocat") {
							t.Fatalf("message = %#v", message)
						}
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
