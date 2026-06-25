package relay

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func exchange(t *testing.T, r *Relay, code string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/enroll/exchange", strings.NewReader(`{"code":"`+code+`"}`))
	w := httptest.NewRecorder()
	r.handleEnrollExchange(w, req)
	return w
}

func TestEnrollExchangeOneTimeAndExpiry(t *testing.T) {
	r := New(EnvTokenStore(""))
	r.enrollCodes["GOOD-2345"] = &enrollCode{token: "wanctl_secret", namespace: "alice", expires: time.Now().Add(time.Minute)}
	r.enrollCodes["OLD-6789"] = &enrollCode{token: "wanctl_old", namespace: "bob", expires: time.Now().Add(-time.Minute)}

	// Valid code -> token + namespace.
	w := exchange(t, r, "GOOD-2345")
	if w.Code != 200 {
		t.Fatalf("valid exchange status=%d body=%s", w.Code, w.Body.String())
	}
	var out struct{ Token, Namespace string }
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.Token != "wanctl_secret" || out.Namespace != "alice" {
		t.Fatalf("got %+v", out)
	}

	// Single-use: the same code must not work twice.
	if w2 := exchange(t, r, "GOOD-2345"); w2.Code == 200 {
		t.Fatal("enrollment code was reusable")
	}

	// Expired code rejected.
	if w3 := exchange(t, r, "OLD-6789"); w3.Code == 200 {
		t.Fatal("expired code accepted")
	}

	// Unknown code rejected.
	if w4 := exchange(t, r, "NOPE-0000"); w4.Code == 200 {
		t.Fatal("unknown code accepted")
	}
}

func TestNewEnrollCodeFormat(t *testing.T) {
	c := newEnrollCode()
	if len(c) != 9 || c[4] != '-' {
		t.Fatalf("bad code format: %q", c)
	}
}
