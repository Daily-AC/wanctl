package portal

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGitHubProxyIsScopedToOAuth(t *testing.T) {
	calls := []string{}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Host+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/login/oauth/access_token" {
			io.WriteString(w, `{"access_token":"test-only"}`)
		} else {
			io.WriteString(w, `{"id":42,"login":"test"}`)
		}
	}))
	defer proxy.Close()
	tr, err := GitHubProxyTransport(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{GitHubTransport: tr, GitHubAuthBase: "http://github.test", GitHubAPIBase: "http://api.github.test"})
	if s.hc.Transport != nil {
		t.Fatal("proxy leaked into general HTTP client")
	}
	user, err := s.githubUserForCode(httptest.NewRequest("GET", "http://portal.test/auth/callback", nil), "test-code")
	if err != nil || user == nil || user.login != "test" {
		t.Fatalf("OAuth through proxy: %v", err)
	}
	if strings.Join(calls, ",") != "github.test/login/oauth/access_token,api.github.test/user" {
		t.Fatalf("wrong proxy requests: %v", calls)
	}
	for _, raw := range []string{"garbage", "file:///tmp/socket", "http://proxy/x", "http://proxy?x=y"} {
		if _, err := GitHubProxyTransport(raw); err == nil {
			t.Errorf("accepted invalid proxy %q", raw)
		}
	}
}
