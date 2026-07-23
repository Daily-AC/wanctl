package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveTokenNamespaceUsesRelayIdentity(t *testing.T) {
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/peers" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		gotToken = r.URL.Query().Get("token")
		json.NewEncoder(w).Encode(map[string]any{"namespace": "real-owner", "devices": []string{}})
	}))
	defer srv.Close()

	ns, err := ResolveTokenNamespace(context.Background(), srv.URL, "raw token/+", "ws")
	if err != nil {
		t.Fatal(err)
	}
	if ns != "real-owner" || gotToken != "raw token/+" {
		t.Fatalf("namespace = %q, token = %q", ns, gotToken)
	}
}

func TestResolveTokenNamespaceRejectsInvalidToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, err := ResolveTokenNamespace(context.Background(), srv.URL, "revoked", "ws"); err == nil {
		t.Fatal("invalid token was accepted")
	}
}
