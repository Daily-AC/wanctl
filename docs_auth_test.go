package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRelayPOSTUsesBearerHeaderWithoutTokenURL(t *testing.T) {
	const token = "docs-secret"
	var requestURI, authorization string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requestURI = req.RequestURI
		authorization = req.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("WANCTL_RELAY", srv.URL)
	t.Setenv("WANCTL_TOKEN", token)

	if err := relayPOST(context.Background(), "/docs/articles?draft=true", map[string]string{"title": "test"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(requestURI, token) || strings.Contains(requestURI, "token=") {
		t.Fatal("credential appeared in docs request URI")
	}
	if !strings.Contains(requestURI, "draft=true") {
		t.Fatal("business query parameter was not preserved")
	}
	if authorization != "Bearer "+token {
		t.Fatal("docs request did not carry the expected bearer credential")
	}
}
