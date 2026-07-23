package relay

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func getInstaller(t *testing.T, srv *httptest.Server, path string) string {
	t.Helper()
	req, err := http.NewRequest("GET", srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET %s: status %d", path, resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

// TestInstallPS1 renders /install.ps1 through the real handler chain and
// asserts the script is well-formed PowerShell: format verbs got substituted,
// the relay base URL is embedded, the portal fingerprint flows through, and
// the cross-platform env-var contract is preserved.
func TestInstallPS1(t *testing.T) {
	fp := distTestFP(1)
	t.Setenv("WANCTL_PORTAL_FP", fp)
	t.Setenv("WANCTL_PUBLIC_URL", "https://downloads.example.test")
	srv := httptest.NewServer(New(EnvTokenStore("")).Handler())
	defer srv.Close()

	s := getInstaller(t, srv, "/install.ps1")

	for _, bad := range []string{"%[1]s", "%[2]s", "%!", "%!(EXTRA"} {
		if strings.Contains(s, bad) {
			t.Errorf("rendered script still contains %q (unrendered format verb)", bad)
		}
	}
	for _, want := range []string{
		"https://downloads.example.test",
		fp,
		"$env:WANCTL_TOKEN",
		"$env:WANCTL_NAME",
		"$env:WANCTL_MODE",
		"$env:WANCTL_INSTALL_ONLY",
		"wanctl-windows-amd64.exe",
		"LOCALAPPDATA",
		"Invoke-WebRequest",
		"'--transport','ws'",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered script missing %q", want)
		}
	}
}

// TestInstallSh keeps the sh installer honest in the same way: format verbs
// get substituted and the relay URL + fingerprint flow through.
func TestInstallSh(t *testing.T) {
	fp := distTestFP(2)
	t.Setenv("WANCTL_PORTAL_FP", fp)
	t.Setenv("WANCTL_PUBLIC_URL", "https://downloads.example.test")
	srv := httptest.NewServer(New(EnvTokenStore("")).Handler())
	defer srv.Close()
	s := getInstaller(t, srv, "/install.sh")
	if !strings.Contains(s, fp) || !strings.Contains(s, "https://downloads.example.test") {
		t.Errorf("sh installer missing relay/fingerprint substitution; got:\n%s", s)
	}
	if !strings.Contains(s, `--transport ws`) {
		t.Errorf("sh installer does not default to WebSocket transport")
	}
	if strings.Contains(s, "%[1]s") || strings.Contains(s, "%[2]s") {
		t.Errorf("sh installer has unrendered verbs")
	}
}

func TestInstallersSeedMultiplePortalFingerprints(t *testing.T) {
	t.Setenv("WANCTL_PORTAL_FP", "")
	old, newFP := distTestFP(3), distTestFP(4)
	t.Setenv("WANCTL_PORTAL_FPS", old+","+newFP)
	srv := httptest.NewServer(New(EnvTokenStore("")).Handler())
	defer srv.Close()
	for _, path := range []string{"/install.sh", "/install.ps1"} {
		script := getInstaller(t, srv, path)
		for _, want := range []string{old, newFP, "portal-admins", "seed"} {
			if !strings.Contains(script, want) {
				t.Errorf("%s missing %q", path, want)
			}
		}
	}
}

func TestInstallerIgnoresHostAndForwardedProto(t *testing.T) {
	t.Setenv("WANCTL_PUBLIC_URL", "https://downloads.example.test")
	r := New(EnvTokenStore(""))
	for _, path := range []string{"/install.sh", "/install.ps1"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "evil.example"
		req.Header.Set("X-Forwarded-Proto", "javascript")
		rec := httptest.NewRecorder()
		r.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "https://downloads.example.test") || strings.Contains(body, "evil.example") || strings.Contains(body, "javascript") {
			t.Errorf("%s used request-controlled public URL: %s", path, body)
		}
	}
}

func TestInstallerRejectsInvalidPortalFingerprints(t *testing.T) {
	t.Setenv("WANCTL_PORTAL_FPS", "SHA256:not-valid-base64")
	srv := httptest.NewServer(New(EnvTokenStore("")).Handler())
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("invalid installer config status = %d, want 500", resp.StatusCode)
	}
}

func TestInstallerRejectsInvalidPublicURL(t *testing.T) {
	for _, value := range []string{
		"javascript://downloads.example.test",
		"https://user:pass@downloads.example.test",
		"https://downloads.example.test/path",
		"https://downloads.example.test?next=https://evil.example",
		"https://downloads.example.test$(touch-pwned)",
		"https://downloads.example.test:99999",
		"//downloads.example.test",
	} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("WANCTL_PUBLIC_URL", value)
			rec := httptest.NewRecorder()
			New(EnvTokenStore("")).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/install.sh", nil))
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("invalid WANCTL_PUBLIC_URL %q = %d, want 503", value, rec.Code)
			}
		})
	}
}

func TestInstallerUsesExplicitCompiledDefault(t *testing.T) {
	t.Setenv("WANCTL_PUBLIC_URL", "")
	got, err := installerPublicURL()
	if err != nil {
		t.Fatal(err)
	}
	if got != defaultPublicURL {
		t.Fatalf("default public URL = %q, want %q", got, defaultPublicURL)
	}
}

func distTestFP(value byte) string {
	return "SHA256:" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string([]byte{value}), 32)))
}
