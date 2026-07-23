package relay

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
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

	for _, path := range []string{"/dl/manifest.json", "/dl/manifest.json.sig", "/dl/wanctl-linux-amd64"} {
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

func TestRelayInstallerIsRetired(t *testing.T) {
	dir := signedDist(t)
	t.Setenv("WANCTL_DIST_DIR", dir)
	srv := httptest.NewServer(New(EnvTokenStore("")).Handler())
	defer srv.Close()
	for _, path := range []string{"/install.sh", "/install.ps1"} {
		resp, err := srv.Client().Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusGone || !strings.Contains(string(body), "cannot bootstrap trust") {
			t.Errorf("GET %s = %d %q", path, resp.StatusCode, body)
		}
	}
}
