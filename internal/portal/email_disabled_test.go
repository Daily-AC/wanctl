package portal

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDisabledAccountCannotSendContactMail(t *testing.T) {
	d := &doorRelay{confirmed: "original@example.com"}
	s, mail := doorPortal(t, d)
	cookie := signedIn(t, s)
	inner := s.hc.Transport
	s.hc.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/admin/resolve-user" {
			rr := httptest.NewRecorder()
			http.Error(rr, accountDisabledBody, http.StatusForbidden)
			return rr.Result(), nil
		}
		return inner.RoundTrip(req)
	})
	h := s.Handler()
	if rr := get(h, "/api/me", cookie); rr.Code != http.StatusForbidden {
		t.Fatalf("fixture did not disable the account: %d %s", rr.Code, rr.Body.String())
	}
	rr := post(h, "/auth/email/send", `{"address":"victim@example.com"}`, cookie)
	if rr.Code != http.StatusForbidden || len(mail.messages) != 0 {
		t.Fatalf("disabled portal session sent contact mail: status=%d delivered=%d body=%s; want 403 and no mail", rr.Code, len(mail.messages), rr.Body.String())
	}
	for _, endpoint := range []string{"/auth/email/status", "/auth/email/cancel"} {
		rr := get(h, endpoint, cookie)
		if endpoint == "/auth/email/cancel" {
			rr = post(h, endpoint, `{}`, cookie)
		}
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s returned %d", endpoint, rr.Code)
		}
	}

}
