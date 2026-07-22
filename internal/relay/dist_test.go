package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// getInstaller fetches /install.{sh,ps1} from the test server while passing
// X-Forwarded-Proto=http so the rendered RELAY URL matches httptest's plain
// HTTP origin (in production nginx sets this header for us).
func getInstaller(t *testing.T, srv *httptest.Server, path string) string {
	t.Helper()
	req, err := http.NewRequest("GET", srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-Proto", "http")
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
	t.Setenv("WANCTL_PORTAL_FP", "sha256:DEADBEEF")
	srv := httptest.NewServer(New(EnvTokenStore("")).Handler())
	defer srv.Close()

	s := getInstaller(t, srv, "/install.ps1")

	for _, bad := range []string{"%[1]s", "%[2]s", "%!", "%!(EXTRA"} {
		if strings.Contains(s, bad) {
			t.Errorf("rendered script still contains %q (unrendered format verb)", bad)
		}
	}
	for _, want := range []string{
		srv.URL,
		"DEADBEEF",
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
	t.Setenv("WANCTL_PORTAL_FP", "sha256:CAFEBABE")
	srv := httptest.NewServer(New(EnvTokenStore("")).Handler())
	defer srv.Close()
	s := getInstaller(t, srv, "/install.sh")
	if !strings.Contains(s, "CAFEBABE") || !strings.Contains(s, srv.URL) {
		t.Errorf("sh installer missing relay/fingerprint substitution; got:\n%s", s)
	}
	if !strings.Contains(s, `--transport ws`) {
		t.Errorf("sh installer does not default to WebSocket transport")
	}
	if strings.Contains(s, "%[1]s") || strings.Contains(s, "%[2]s") {
		t.Errorf("sh installer has unrendered verbs")
	}
}
