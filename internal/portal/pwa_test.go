package portal

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/png"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

type webManifest struct {
	Name       string                                       `json:"name"`
	ShortName  string                                       `json:"short_name"`
	StartURL   string                                       `json:"start_url"`
	Scope      string                                       `json:"scope"`
	Display    string                                       `json:"display"`
	Theme      string                                       `json:"theme_color"`
	Background string                                       `json:"background_color"`
	Icons      []struct{ Src, Sizes, Type, Purpose string } `json:"icons"`
}

func readManifest(t *testing.T) webManifest {
	t.Helper()
	rec := httptest.NewRecorder()
	New(Config{}).Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/manifest.webmanifest", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/manifest+json" {
		t.Fatalf("manifest: status %d, content type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var m webManifest
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPWAManifestAndIcons(t *testing.T) {
	m := readManifest(t)
	if m.Name != "wanctl" || m.ShortName != "wanctl" || m.Display != "standalone" || m.StartURL != "/" || m.Scope != "/" {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	canvas := regexp.MustCompile(`--canvas:(#[a-fA-F0-9]+);`).FindStringSubmatch(readWeb(t, "web/app.css"))[1]
	if m.Theme != canvas || m.Background != canvas {
		t.Fatal("manifest colors differ from canvas token")
	}
	want := map[string]int{"any/192x192": 192, "any/512x512": 512, "maskable/512x512": 512}
	for _, icon := range m.Icons {
		key := icon.Purpose + "/" + icon.Sizes
		size, ok := want[key]
		if !ok || icon.Type != "image/png" {
			t.Fatalf("unexpected icon: %+v", icon)
		}
		delete(want, key)
		checkPWAIcon(t, icon.Src, size, icon.Purpose == "maskable")
	}
	if len(want) != 0 {
		t.Fatalf("missing icons: %v", want)
	}
	checkPWAIcon(t, "/assets/icons/apple-touch-icon.png?v="+assetVersion, 180, false)
}

func checkPWAIcon(t *testing.T, src string, size int, maskable bool) {
	t.Helper()
	u, err := url.Parse(src)
	if err != nil || u.IsAbs() || !strings.HasPrefix(u.Path, "/assets/icons/") || u.Query().Get("v") != assetVersion {
		t.Fatalf("invalid versioned icon URL %q", src)
	}
	rec := httptest.NewRecorder()
	New(Config{}).Handler().ServeHTTP(rec, httptest.NewRequest("GET", src, nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" || rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("icon %s: status %d headers %v", src, rec.Code, rec.Header())
	}
	im, err := png.Decode(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if im.Bounds().Dx() != size || im.Bounds().Dy() != size {
		t.Fatalf("%s: got %v, want %dx%d", src, im.Bounds(), size, size)
	}
	if maskable {
		// Every pixel is opaque; every mark pixel fits the spec's radius-40% safe circle.
		mark := 0
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				r, g, b, a := im.At(x, y).RGBA()
				if a != 65535 {
					t.Fatalf("transparent maskable pixel at %d,%d", x, y)
				}
				if r > 0x3000 && g > 0x3000 && b > 0x3000 {
					mark++
					if math.Hypot(float64(x)+.5-float64(size)/2, float64(y)+.5-float64(size)/2) > float64(size)*.4 {
						t.Fatalf("mark outside maskable safe circle at %d,%d", x, y)
					}
				}
			}
		}
		if mark == 0 {
			t.Fatal("maskable icon has no mark")
		}
	}
}

func TestEveryPortalPageHasPWAHead(t *testing.T) {
	// Mail templates are email bodies, not browser routes. Enumerate the actual
	// parsed page templates so adding a new portal page also adds it to this test.
	rendered := map[string]string{"index.html": string(indexPage)}
	for _, page := range pages.Templates() {
		name := page.Name()
		if !strings.HasSuffix(name, ".html") {
			continue
		}
		data := map[string]any{"Request": map[string]any{}, "Status": "approved"}
		if name == "delegation-error.html" {
			data["Status"] = 403
		}
		rendered[name] = renderPage(t, name, data)
	}
	for name, body := range rendered {
		t.Run(name, func(t *testing.T) {
			head := regexp.MustCompile(`(?s)<head>(.*?)</head>`).FindStringSubmatch(body)
			if len(head) != 2 {
				t.Fatal("missing head")
			}
			for _, tag := range []string{
				`<link rel="manifest" href="/manifest.webmanifest">`,
				`<meta name="theme-color" content="#ffffff">`,
				fmt.Sprintf(`<link rel="apple-touch-icon" sizes="180x180" href="/assets/icons/apple-touch-icon.png?v=%s">`, assetVersion),
				`<meta name="apple-mobile-web-app-title" content="wanctl">`,
				`<meta name="mobile-web-app-capable" content="yes">`,
				`<meta name="apple-mobile-web-app-capable" content="yes">`,
				`<meta name="apple-mobile-web-app-status-bar-style" content="default">`,
				`<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">`,
			} {
				if !strings.Contains(head[1], tag) {
					t.Errorf("missing %s", tag)
				}
			}
		})
	}
}

func TestPWALoginStaysInScope(t *testing.T) {
	m := readManifest(t)
	s := newOAuthPortal(t, resolveOKAs("octocat", "admin"))
	h := s.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", m.StartURL, nil))
	login, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || login.IsAbs() || login.Path != "/auth/login" || !strings.HasPrefix(login.Path, m.Scope) {
		t.Fatalf("login redirect %v", login)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", login.String(), nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `href="/auth/github?next=%2F"`) {
		t.Fatal("login entry missing")
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "https://wanctl.example/auth/github?next=%2F", nil))
	github, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	callback, err := url.Parse(github.Query().Get("redirect_uri"))
	if err != nil || callback.Host != "wanctl.example" || callback.Path != "/auth/callback" || !strings.HasPrefix(callback.Path, m.Scope) {
		t.Fatalf("callback %v", callback)
	}
	// Existing OAuth helper exercises token exchange and session issuance with
	// simulated upstreams. This cannot model iOS's separate browser cookie jars.
	rec = loginThroughCallback(t, s, m.StartURL)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != m.StartURL {
		t.Fatal("callback did not return to start_url")
	}
	req := httptest.NewRequest("GET", m.StartURL, nil)
	req.AddCookie(sessionCookie(t, rec))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `rel="manifest"`) {
		t.Fatal("session did not open portal")
	}
}
