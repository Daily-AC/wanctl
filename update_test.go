package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"wanctl/internal/config"
	wanrelease "wanctl/internal/release"
)

// signedUpdateServer serves one release the way a relay's /dl mirror or a
// GitHub release page does: manifest, detached signature, artifact. The OS,
// arch and version are parameters because the auto-updater downloads for the
// platform it is actually running on, so its tests cannot use a fixture that is
// permanently linux/amd64.
func signedUpdateServer(t *testing.T, payload []byte, goos, goarch, version string, mutate func(path string, body []byte) []byte) *httptest.Server {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wanrelease.TrustedPublicKeys = base64.StdEncoding.EncodeToString(pub)
	t.Cleanup(func() { wanrelease.TrustedPublicKeys = "" })
	h := sha256.Sum256(payload)
	artifact := wanrelease.ArtifactName(goos, goarch)
	manifest, err := json.Marshal(wanrelease.Manifest{
		Schema: 1, Version: version, PublishedAt: time.Now().UTC(),
		Artifacts: []wanrelease.Artifact{{OS: goos, Arch: goarch, Name: artifact, Size: int64(len(payload)), SHA256: hex.EncodeToString(h[:])}},
	})
	if err != nil {
		t.Fatal(err)
	}
	signature := ed25519.Sign(priv, manifest)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body []byte
		switch req.URL.Path {
		case "/dl/" + wanrelease.ManifestName:
			body = manifest
		case "/dl/" + wanrelease.SignatureName:
			body = signature
		case "/dl/" + artifact:
			body = payload
		default:
			http.NotFound(w, req)
			return
		}
		if mutate != nil {
			body = mutate(req.URL.Path, body)
		}
		w.Write(body)
	}))
}

func TestDownloadSignedUpdateVerifiesBeforeReturning(t *testing.T) {
	srv := signedUpdateServer(t, []byte("signed binary"), "linux", "amd64", "v2.0.0", nil)
	defer srv.Close()
	dir := t.TempDir()
	path, version, err := downloadSignedUpdate(t.Context(), srv.URL+"/dl", dir, "linux", "amd64", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	if version != "v2.0.0" {
		t.Fatalf("version = %q", version)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "signed binary" {
		t.Fatalf("downloaded = %q, %v", got, err)
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("temporary artifact is outside destination directory: %s", path)
	}
}

func TestDownloadSignedUpdateRejectsTamperedArtifact(t *testing.T) {
	srv := signedUpdateServer(t, []byte("signed binary"), "linux", "amd64", "v2.0.0", func(path string, body []byte) []byte {
		if path == "/dl/wanctl-linux-amd64" {
			return []byte("attacker binary")
		}
		return body
	})
	defer srv.Close()
	dir := t.TempDir()
	if _, _, err := downloadSignedUpdate(t.Context(), srv.URL+"/dl", dir, "linux", "amd64", "v1.0.0"); err == nil {
		t.Fatal("tampered artifact accepted")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("failed update left files behind: %v", entries)
	}
}

func TestPlanUpdateRestartPreservesSupervisorOwnership(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WANCTL_CONFIG_DIR", dir)
	const pid = 4242
	if err := config.WriteManagedPID(pid); err != nil {
		t.Fatal(err)
	}

	plan := planUpdateRestartWithLiveness(false, pid, true)
	if plan.stopDetached || plan.restartDetached || plan.restartManagedPID != pid {
		t.Fatalf("managed plan = %+v", plan)
	}

	if err := config.WriteManagedPID(pid + 1); err != nil {
		t.Fatal(err)
	}
	plan = planUpdateRestartWithLiveness(false, pid, true)
	if !plan.stopDetached || !plan.restartDetached || plan.restartManagedPID != 0 {
		t.Fatalf("stale managed marker plan = %+v", plan)
	}
}
