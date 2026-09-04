package portal

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The three pre-app pages are the only HTML the portal renders itself, and two
// of them are the first wanctl interface a person ever sees (the sign-in page
// when they arrive from the website, the enroll page when they run wanctl on a
// new machine). Nothing else builds them, so these are the checks.

func renderPage(t *testing.T, name string, data map[string]any) string {
	t.Helper()
	s := New(Config{})
	rec := httptest.NewRecorder()
	s.render(rec, name, data)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status %d — %s", name, rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestPagesRenderCompletely(t *testing.T) {
	cases := []struct {
		name string
		data map[string]any
		want []string
	}{
		{"login.html", map[string]any{"Host": "wanctl.example:8443", "Start": "/auth/github?next=%2Fenroll"},
			[]string{"wanctl.example:8443", "/auth/github?next=%2Fenroll", "invite-only"}},
		{"pending.html", map[string]any{"Login": "octocat"},
			[]string{"octocat", "/assets/auth.js"}},
		{"enroll.html", map[string]any{"NS": "octocat", "Code": "7F3K9QP2", "Mins": 10, "FP": "ab12cd34"},
			[]string{"7F3K9QP2", "octocat", ">10<", "ab12cd34"}},
	}
	for _, c := range cases {
		out := renderPage(t, c.name, c.data)
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("%s does not contain %q", c.name, w)
			}
		}
		// An unfilled placeholder renders as nothing at all, so the page still
		// looks fine — this is the only thing that would tell you.
		if strings.Contains(out, "{{") {
			t.Errorf("%s still contains an unexecuted template action", c.name)
		}
		// Every page hangs off the app's stylesheet and the shared script, and
		// both must carry the asset fingerprint rather than the placeholder.
		for _, w := range []string{"/assets/app.css?v=" + assetVersion, "/assets/auth.js?v=" + assetVersion} {
			if !strings.Contains(out, w) {
				t.Errorf("%s does not request %s", c.name, w)
			}
		}
	}
}

// The GitHub login is the one value on these pages that comes from outside.
// The old pending page was fmt.Fprintf with a hand-rolled escaper; this is why
// the pages moved to html/template.
func TestPendingPageEscapesTheLogin(t *testing.T) {
	out := renderPage(t, "pending.html", map[string]any{"Login": `<img src=x onerror="boom()">`})
	if strings.Contains(out, "<img src=x") {
		t.Fatal("the login reached the page as markup")
	}
	if !strings.Contains(out, "&lt;img") {
		t.Fatal("the login was not escaped into the page at all")
	}
}

func TestLoginPageIsAPageAndNotARedirect(t *testing.T) {
	s := newOAuthPortal(t, resolveOKAs("octocat", "user"))
	rec := httptest.NewRecorder()
	s.handleAuthLogin(rec, httptest.NewRequest("GET", "/auth/login?next=%2Fenroll", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 — the sign-in page exists so that nobody is "+
			"handed to github.com without being shown which instance is asking", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("sign-in page redirects to %q", loc)
	}
	body := rec.Body.String()
	// next has to survive the extra hop, or `wanctl` on a new machine lands in
	// the app instead of on the enroll page it opened.
	if !strings.Contains(body, "/auth/github?next=%2Fenroll") {
		t.Error("the sign-in button lost the next parameter")
	}
	if strings.Contains(body, "gh.test") {
		t.Error("the page links straight to GitHub; state must be minted at /auth/github")
	}
}

// An external next must not survive as a link on the page either — the
// collapse happens on the way in, not only on the way back.
func TestLoginPageRejectsOpenRedirect(t *testing.T) {
	s := newOAuthPortal(t, resolveOKAs("octocat", "user"))
	rec := httptest.NewRecorder()
	s.handleAuthLogin(rec, httptest.NewRequest("GET", "/auth/login?next=https%3A%2F%2Fevil.example", nil))
	if got := rec.Body.String(); !strings.Contains(got, `href="/auth/github?next=%2F"`) {
		t.Fatal("external next was not collapsed to / in the sign-in link")
	}
}

func TestLoginPageSkippedWhenAlreadySignedIn(t *testing.T) {
	s := newOAuthPortal(t, resolveOKAs("octocat", "user"))
	rec := loginThroughCallback(t, s, "/")
	req := httptest.NewRequest("GET", "/auth/login?next=%2Fenroll", nil)
	req.AddCookie(sessionCookie(t, rec))
	rec2 := httptest.NewRecorder()
	s.handleAuthLogin(rec2, req)
	if rec2.Code != http.StatusSeeOther || rec2.Header().Get("Location") != "/enroll" {
		t.Fatalf("signed-in visitor: status %d location %q, want 303 /enroll",
			rec2.Code, rec2.Header().Get("Location"))
	}
}

// Header (SSO) deployments have no OAuth, so neither page exists there.
func TestAuthPagesAbsentInHeaderMode(t *testing.T) {
	s := New(Config{RelayAdminURL: "https://relay.test", AdminSecret: "x"})
	for path, h := range map[string]http.HandlerFunc{
		"/auth/login":  s.handleAuthLogin,
		"/auth/github": s.handleAuthStart,
		"/pending":     s.handlePending,
	} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s in header mode: status %d, want 404", path, rec.Code)
		}
	}
}

// web/ holds the page templates next to the stylesheet, and /assets/ serves
// that directory. It must not hand out either those or index.html, both of
// which still carry their placeholders.
func TestAssetsRefuseHTML(t *testing.T) {
	s := New(Config{})
	for _, name := range []string{"index.html", "login.html", "pending.html", "enroll.html"} {
		rec := httptest.NewRecorder()
		s.handleAsset(rec, httptest.NewRequest("GET", "/assets/"+name, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("/assets/%s: status %d, want 404", name, rec.Code)
		}
	}
	// The things it does serve still come out.
	for _, name := range []string{"app.css", "app.js", "auth.js", "mark.svg"} {
		rec := httptest.NewRecorder()
		s.handleAsset(rec, httptest.NewRequest("GET", "/assets/"+name, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("/assets/%s: status %d, want 200", name, rec.Code)
		}
	}
}
