package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wanctl/internal/policy"
)

// A dropped poll is a network failure, whatever its URL happens to contain.
// The error text of a failed request carries the whole poll URL — device ID,
// instance, fingerprint, name — so matching "401" in it took an agent down for
// good on an ordinary timeout whenever one of those spelled 401 somewhere. A
// real rejection arrives as a status code, and only that may end the loop.
func TestPollTransportErrorIsNotATokenRejection(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.HasPrefix(req.URL.Path, "/h/poll") {
			return
		}
		if polls.Add(1) == 1 {
			// Drop the connection without an answer: a transport error.
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	a, err := New(Options{RelayURL: srv.URL, Token: "synthetic", Mode: policy.ModeNormal, Name: "box-401"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = a.runHTTP(ctx)
	if n := polls.Load(); n < 2 {
		t.Fatalf("agent stopped after %d poll(s) with %v; a dropped connection must be retried", n, err)
	}
	if err == nil || !strings.Contains(err.Error(), "(401)") {
		t.Fatalf("runHTTP = %v, want the relay's real 401 to end it", err)
	}
}
