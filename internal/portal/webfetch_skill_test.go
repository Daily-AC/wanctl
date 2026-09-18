package portal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// skillPortal answers discovery the way a relay with WebFetch enabled does.
func skillPortal(t *testing.T, enabled bool) *Server {
	t.Helper()
	return newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/webfetch/v1" {
			t.Fatalf("the skill route must not call anything but discovery: %s", r.URL.Path)
		}
		if !enabled {
			// A relay built without a seed serves no adapter at all.
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{
			"protocol":           "wanctl.webfetch.v1",
			"start_url_template": "https://relay.test/webfetch/new/{client_nonce}",
		})
	})
}

func fetchSkill(t *testing.T, s *Server, query string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	// Deliberately no session cookie and no identity header: the AI that loads
	// this file has neither.
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/webfetch/skill"+query, nil))
	return w
}

func TestWebFetchSkillIsPublicMarkdownForThisInstance(t *testing.T) {
	w := fetchSkill(t, skillPortal(t, true), "")
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

func TestWebFetchSkillJSONCarriesTheSameMarkdown(t *testing.T) {
	s := skillPortal(t, true)
	markdown := fetchSkill(t, s, "").Body.String()
	w := fetchSkill(t, s, "?format=json")
	var out struct {
		Protocol string `json:"protocol"`
		Skill    string `json:"skill"`
		SkillURL string `json:"skill_url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("format=json is not JSON: %v", err)
	}
	if out.Protocol != "wanctl.webfetch.v1" || out.Skill != markdown {
		t.Errorf("json skill = %q, %d bytes; markdown is %d bytes", out.Protocol, len(out.Skill), len(markdown))
	}
	if out.SkillURL != "http://example.com/webfetch/skill" {
		t.Errorf("skill_url = %q", out.SkillURL)
	}
}

// Nothing here works without the adapter, so the route says so in the only way
// a URL reader understands rather than serving instructions that cannot run.
func TestWebFetchSkillIs404WhenWebFetchIsDisabled(t *testing.T) {
	w := fetchSkill(t, skillPortal(t, false), "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("skill with WebFetch disabled = %d, want 404", w.Code)
	}
	if strings.Contains(w.Body.String(), "wanctl-webfetch") {
		t.Error("the disabled route still served the skill")
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
