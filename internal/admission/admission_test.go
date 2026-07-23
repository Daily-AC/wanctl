package admission

import (
	"net/http/httptest"
	"testing"
)

func TestTokenPrefersBearerAndFailsClosed(t *testing.T) {
	req := httptest.NewRequest("GET", "/?token=legacy", nil)
	req.Header.Set("Authorization", "Bearer current")
	if token, legacy, ok := Token(req); token != "current" || legacy || !ok {
		t.Fatalf("Token = %q, legacy=%v, ok=%v", token, legacy, ok)
	}
	req.Header.Set("Authorization", "not-bearer")
	if _, _, ok := Token(req); ok {
		t.Fatal("malformed Authorization fell back to query token")
	}
}

func TestTokenLegacyQuery(t *testing.T) {
	req := httptest.NewRequest("GET", "/?token=legacy", nil)
	if token, legacy, ok := Token(req); token != "legacy" || !legacy || !ok {
		t.Fatalf("Token = %q, legacy=%v, ok=%v", token, legacy, ok)
	}
}
