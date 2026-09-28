package relay

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// bearerFromQuery moves a test request's token= parameter into the
// Authorization header, the one place the relay reads a credential from, so
// fixtures can keep spelling a whole request as one query string.
func bearerFromQuery(req *http.Request) {
	q := req.URL.Query()
	if token := q.Get("token"); token != "" {
		q.Del("token")
		req.URL.RawQuery = q.Encode()
		req.RequestURI = req.URL.RequestURI()
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// A credential travels in the Authorization header and nowhere else. A token in
// the URL is written into every access log between the caller and the relay,
// and into browser history and Referer headers besides.
func TestTokenInQueryIsNotAccepted(t *testing.T) {
	h := New(EnvTokenStore("secret-token:alice")).Handler()
	for _, path := range []string{
		"/peers?token=secret-token",                      // controller endpoints
		"/resolve?token=secret-token&target=alice/box",   // (delegation-aware admission)
		"/agent?token=secret-token",                      // device registration
		"/u/friends?token=secret-token",                  // account endpoints
		"/h/peers?token=secret-token",                    // HTTP transport
		"/h/dial?token=secret-token&target=alice/device", // HTTP transport
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("GET %s = %d, want 401", path, rr.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/peers", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("bearer request = %d, want 200", rr.Code)
	}
}
