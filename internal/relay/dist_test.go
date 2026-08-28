package relay

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	wanrelease "wanctl/internal/release"
)

func TestSkillsUsesForwardedOrigin(t *testing.T) {
	t.Setenv("WANCTL_PUBLIC_ORIGIN", "")
	req := httptest.NewRequest(http.MethodGet, "http://relay.internal/skills", nil)
	req.Host = "relay.example"
	req.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()

	New(EnvTokenStore("")).handleSkills(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "@WANCTL_RELAY@") {
		t.Fatal("response still contains relay placeholder")
	}
	if !strings.Contains(rec.Body.String(), "https://relay.example/skills") {
		t.Fatalf("response does not use forwarded origin: %s", rec.Body.String())
	}
}

func TestSkillsUsesDirectTLSOrigin(t *testing.T) {
	t.Setenv("WANCTL_PUBLIC_ORIGIN", "")
	req := httptest.NewRequest(http.MethodGet, "https://relay.direct/skills", nil)
	req.Host = "relay.direct:8443"
	req.TLS = &tls.ConnectionState{}
	rec := httptest.NewRecorder()

	New(EnvTokenStore("")).handleSkills(rec, req)

	if strings.Contains(rec.Body.String(), "@WANCTL_RELAY@") {
		t.Fatal("response still contains relay placeholder")
	}
	if !strings.Contains(rec.Body.String(), "https://relay.direct:8443/skills") {
		t.Fatalf("response does not use direct TLS origin: %s", rec.Body.String())
	}
}

func signedDist(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	payload := []byte("signed binary")
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

func TestDistributionVerificationConcurrencyIsBounded(t *testing.T) {
	dir := signedDist(t)
	handler, err := newSignedDistHandler(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := handler.(*signedDistHandler)
	h.verifySlots <- struct{}{}
	h.verifySlots <- struct{}{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/wanctl-linux-amd64", nil)
	req.URL.Path = "wanctl-linux-amd64" // signedDistHandler runs behind StripPrefix("/dl/").
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("busy distribution status = %d, want 503", rec.Code)
	}
}

// slotProbe records how many verification slots were held at the moment the
// first body byte was written, i.e. once streaming to the client has begun.
type slotProbe struct {
	*httptest.ResponseRecorder
	slots    chan struct{}
	heldAt   int
	sawWrite bool
}

func (p *slotProbe) Write(b []byte) (int, error) {
	if !p.sawWrite {
		p.sawWrite = true
		p.heldAt = len(p.slots)
	}
	return p.ResponseRecorder.Write(b)
}

// Two slow readers used to pin both slots for as long as they cared to drain
// the body, answering everyone else with 503 (audit 2026-08-28, SEC-F-07).
func TestDistributionSlotReleasedBeforeStreaming(t *testing.T) {
	dir := signedDist(t)
	handler, err := newSignedDistHandler(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := handler.(*signedDistHandler)
	probe := &slotProbe{ResponseRecorder: httptest.NewRecorder(), slots: h.verifySlots}
	req := httptest.NewRequest(http.MethodGet, "/wanctl-linux-amd64", nil)
	req.URL.Path = "wanctl-linux-amd64"
	h.ServeHTTP(probe, req)
	if probe.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", probe.Code)
	}
	if !probe.sawWrite {
		t.Fatal("no body was written")
	}
	if probe.heldAt != 0 {
		t.Fatalf("%d verification slot(s) still held while streaming, want 0", probe.heldAt)
	}
	if n := len(h.verifySlots); n != 0 {
		t.Fatalf("%d verification slot(s) leaked after the request", n)
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
