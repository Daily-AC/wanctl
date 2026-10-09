package portal

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLog(t *testing.T) *logBuffer {
	t.Helper()
	b, prev := &logBuffer{}, log.Writer()
	log.SetOutput(b)
	t.Cleanup(func() { log.SetOutput(prev) })
	return b
}

// awaitLogLine waits for a line containing want; approval mail is sent in the
// background, after the response.
func awaitLogLine(t *testing.T, logs *logBuffer, want string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(line, want) {
				return line
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no log line with %q in:\n%s", want, logs.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A delivered mail leaves a line too, so "did the confirmation go out?" is
// answered from the portal log instead of the recipient's inbox (10-01). The
// line names the user and the kind of mail and never the address.
func TestDeliveredMailIsLoggedWithoutTheAddress(t *testing.T) {
	t.Run("confirmation", func(t *testing.T) {
		logs := captureLog(t)
		s, mail := doorPortal(t, &doorRelay{resolve: "ok"})
		h := s.Handler()
		if rr := post(h, "/auth/email/send", `{"address":"person@example.com"}`, signedIn(t, s)); rr.Code != 200 {
			t.Fatalf("send = %d %s", rr.Code, rr.Body.String())
		}
		<-mail.messages
		line := awaitLogLine(t, logs, "confirmation mail for octocat sent")
		if strings.Contains(logs.String(), "person@example.com") {
			t.Fatalf("the log carries the address: %s", line)
		}
	})
	t.Run("approval", func(t *testing.T) {
		logs := captureLog(t)
		var calls []string
		s := accessPortal(t, resolveOKAs("admin", "admin"), &calls)
		fake := newFakeMailSender()
		s.mail = fake
		inner := s.hc.Transport
		s.hc.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != "/admin/access-requests/decide" {
				return inner.RoundTrip(r)
			}
			rr := httptest.NewRecorder()
			json.NewEncoder(rr).Encode(map[string]any{"id": 7, "login": "octocat", "email": "applicant@example.com", "status": "approved"})
			return rr.Result(), nil
		})
		h := s.Handler()
		cookies := inviteSession(t, s, h)
		req := httptest.NewRequest("POST", "https://portal.test/api/access-requests/decide", strings.NewReader(`{"id":7,"decision":"approved"}`))
		for _, c := range cookies {
			req.AddCookie(c)
		}
		rr := httptest.NewRecorder()
		s.handleAccessDecide(rr, req)
		if rr.Code != 200 {
			t.Fatalf("decide = %d", rr.Code)
		}
		<-fake.messages
		line := awaitLogLine(t, logs, "approval mail for octocat sent (request 7)")
		if strings.Contains(logs.String(), "applicant@example.com") {
			t.Fatalf("the log carries the address: %s", line)
		}
	})
}
