package relay

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wanctl/internal/limits"
)

// Every control endpoint decodes its body with encoding/json, which buffers a
// string value whole; without a cap one 1 GiB POST to the unauthenticated
// /enroll/exchange inflated the relay to 4 GiB (audit 2026-08-28, SEC-B-02).
func TestLimitBodiesCapsControlRoutes(t *testing.T) {
	var got int64
	var readErr error
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		got, readErr = n, err
	})
	h := limitBodies(next)

	oversized := bytes.Repeat([]byte("A"), int(limits.RelayControlBodyBytes)+1)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/enroll/exchange", bytes.NewReader(oversized)))
	var mbe *http.MaxBytesError
	if !errors.As(readErr, &mbe) || got > limits.RelayControlBodyBytes {
		t.Fatalf("/enroll/exchange read %d bytes, err %v; want MaxBytesError at %d", got, readErr, limits.RelayControlBodyBytes)
	}

	// Within the cap nothing changes.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/u/shares/grant", strings.NewReader(`{"device":"d"}`)))
	if readErr != nil || got != int64(len(`{"device":"d"}`)) {
		t.Fatalf("small body: read %d, err %v", got, readErr)
	}

	// The tunnel upload route bounds itself per write and must not be capped
	// a second time at a smaller size.
	big := bytes.Repeat([]byte("B"), int(limits.RelayControlBodyBytes)*4)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/h/up", bytes.NewReader(big)))
	if readErr != nil || got != int64(len(big)) {
		t.Fatalf("/h/up: read %d, err %v; the tunnel route must be exempt", got, readErr)
	}
}

func TestBodyCapPerRoute(t *testing.T) {
	cases := map[string]int64{
		"/h/up":                   0,
		"/h/dial":                 limits.RelayControlBodyBytes,
		"/enroll/exchange":        limits.RelayControlBodyBytes,
		"/admin/tokens/issue":     limits.RelayControlBodyBytes,
		"/admin/docs/articles":    limits.RelayDocsBodyBytes,
		"/docs/articles":          limits.RelayDocsBodyBytes,
		"/wanctl-mcp":             limits.RelayMCPBodyBytes,
		"/wanctl-mcp/session/abc": limits.RelayMCPBodyBytes,
	}
	for path, want := range cases {
		if got := bodyCapFor(path); got != want {
			t.Errorf("%s: cap %d, want %d", path, got, want)
		}
	}
}

// End to end through the real handler: the oversized body is rejected without
// ever being buffered, and the connection is marked for close.
func TestEnrollExchangeRejectsOversizedBody(t *testing.T) {
	r := New(EnvTokenStore("tok:alice"))
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	body := `{"code":"` + strings.Repeat("A", int(limits.RelayControlBodyBytes)) + `"}`
	resp, err := http.Post(srv.URL+"/enroll/exchange", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("oversized exchange returned 200")
	}
}
