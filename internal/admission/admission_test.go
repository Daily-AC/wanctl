package admission

import (
	"net/http/httptest"
	"testing"
)

func TestTokenReadsBearerAndFailsClosed(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer current")
	if token, ok := Token(req); token != "current" || !ok {
		t.Fatalf("Token = %q, ok=%v", token, ok)
	}
	req.Header.Set("Authorization", "bearer   current")
	if token, ok := Token(req); token != "current" || !ok {
		t.Fatalf("case-insensitive scheme: Token = %q, ok=%v", token, ok)
	}
	for _, malformed := range []string{"not-bearer", "Bearer", "Basic current", "Bearer a b"} {
		req.Header.Set("Authorization", malformed)
		if _, ok := Token(req); ok {
			t.Fatalf("malformed Authorization %q accepted", malformed)
		}
	}
}

// A token in the URL is never a credential, with or without a header beside it.
func TestTokenIgnoresTheQuery(t *testing.T) {
	req := httptest.NewRequest("GET", "/?token=from-url", nil)
	if token, ok := Token(req); ok || token != "" {
		t.Fatalf("query token accepted: %q", token)
	}
	req.Header.Set("Authorization", "invalid header")
	if token, ok := Token(req); ok || token != "" {
		t.Fatalf("malformed header fell back to the query token: %q", token)
	}
}
