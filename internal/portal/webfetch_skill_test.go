package portal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// skillPortal answers nothing: like the quick reference, the skill is built
// from configuration, so a public fetch of it must never become a relay
// request.
func skillPortal(t *testing.T, relay string) *Server {
	t.Helper()
	s := newTestPortal(func(_ http.ResponseWriter, r *http.Request) {
		t.Fatalf("the public skill route must not contact the relay: %s", r.URL.Path)
	})
	s.relayPublic = relay
	return s
}

func fetchSkill(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	// Deliberately no session cookie and no identity header: the AI that loads
	// this file has neither.
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/webfetch/skill", nil))
	return w
}

func TestWebFetchSkillIsPublicMarkdownForThisInstance(t *testing.T) {
	w := fetchSkill(t, skillPortal(t, "https://relay.test"))
	body := w.Body.String()
	if w.Code != http.StatusOK || w.Header().Get("Location") != "" {
		t.Fatalf("anonymous skill = %d %s", w.Code, w.Header().Get("Location"))
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
		t.Errorf("content type = %q", ct)
	}
	// Frontmatter a skill host can parse, then this instance's own URLs.
	if !strings.HasPrefix(body, "---\nname: wanctl-webfetch\ndescription: ") {
		t.Errorf("skill does not start with parseable frontmatter naming wanctl-webfetch:\n%.120s", body)
	}
	for _, want := range []string{
		"https://relay.test/webfetch/v1",
		"http://example.com/webfetch/help",
		"http://example.com/webfetch/connect",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("served skill never names %s", want)
		}
	}
	// A template that reaches a reader with a placeholder still in it sends
	// them to a URL that does not exist.
	for _, leftover := range []string{"{{", "@WANCTL"} {
		if strings.Contains(body, leftover) {
			t.Errorf("served skill still carries the placeholder %q", leftover)
		}
	}
	if strings.Contains(body, "wanctl-relay.z10.dev") || strings.Contains(body, "wanctl.z10.dev") {
		t.Error("served skill carries the origins of the instance it was written on")
	}
}

// Without a relay there is no protocol entry to send the reader to, and half a
// skill is worse than none.
func TestWebFetchSkillIs404WithoutARelay(t *testing.T) {
	w := fetchSkill(t, skillPortal(t, ""))
	if w.Code != http.StatusNotFound {
		t.Fatalf("skill without a configured relay = %d, want 404", w.Code)
	}
	if strings.Contains(w.Body.String(), "wanctl-webfetch") {
		t.Error("the unconfigured route still served the skill")
	}
}

// The owner finds the file where they connect their AI, in both languages.
func TestConnectPageOffersTheSkillFile(t *testing.T) {
	s := newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			json.NewEncoder(w).Encode(map[string]string{"namespace": "alice", "role": "user"})
		case "/webfetch/v1":
			json.NewEncoder(w).Encode(map[string]string{"protocol": "wanctl.webfetch.v1", "start_url_template": "https://relay.test/webfetch/new/{client_nonce}"})
		default:
			t.Fatalf("unexpected relay call: %s", r.URL.Path)
		}
	})
	r := httptest.NewRequest(http.MethodGet, "/webfetch/connect", nil)
	r.Header.Set("X-User", "alice@example.com")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	body := w.Body.String()
	for _, want := range []string{
		"http://example.com/webfetch/skill",
		`data-en="Install as a skill"`,
		`data-zh="装成技能"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("connect page is missing %q", want)
		}
	}
}
