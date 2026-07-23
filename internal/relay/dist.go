package relay

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	wanrelease "wanctl/internal/release"
)

//go:embed skill.md
var skillMD []byte

// registerDist exposes only artifacts named by a valid, offline-signed release
// manifest. A missing trust anchor, manifest, signature, or artifact disables
// distribution instead of falling back to unsigned files.
func (r *Relay) registerDist(mux *http.ServeMux) {
	dir := os.Getenv("WANCTL_DIST_DIR")
	if dir == "" {
		dir = "/dist"
	}
	handler, err := newSignedDistHandler(dir)
	if err != nil {
		handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "signed release distribution unavailable", http.StatusServiceUnavailable)
		})
		fmt.Fprintf(os.Stderr, "wanctl relay: release distribution disabled: %v\n", err)
	}
	mux.Handle("/dl/", http.StripPrefix("/dl/", handler))
	mux.HandleFunc("/install.sh", retiredRelayInstaller)
	mux.HandleFunc("/install.ps1", retiredRelayInstaller)
	mux.HandleFunc("/skills", r.handleSkills)
}

func newSignedDistHandler(dir string) (http.Handler, error) {
	manifestRaw, err := os.ReadFile(filepath.Join(dir, wanrelease.ManifestName))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	signatureRaw, err := os.ReadFile(filepath.Join(dir, wanrelease.SignatureName))
	if err != nil {
		return nil, fmt.Errorf("read manifest signature: %w", err)
	}
	manifest, err := wanrelease.VerifyManifest(manifestRaw, signatureRaw, wanrelease.TrustedPublicKeys)
	if err != nil {
		return nil, fmt.Errorf("verify manifest: %w", err)
	}
	if err := wanrelease.VerifyDirectory(dir, manifestRaw, signatureRaw, wanrelease.TrustedPublicKeys); err != nil {
		return nil, err
	}
	artifacts := make(map[string]wanrelease.Artifact)
	for _, artifact := range manifest.Artifacts {
		artifacts[artifact.Name] = artifact
	}
	return &signedDistHandler{
		dir: dir, manifestRaw: manifestRaw, signatureRaw: signatureRaw,
		artifacts: artifacts, verifySlots: make(chan struct{}, 2),
	}, nil
}

type signedDistHandler struct {
	dir          string
	manifestRaw  []byte
	signatureRaw []byte
	artifacts    map[string]wanrelease.Artifact
	verifySlots  chan struct{}
}

func (h *signedDistHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// A deployment changing underneath a running relay is not served partially.
	// The new directory becomes visible only after a relay restart re-verifies it.
	manifestNow, manifestErr := os.ReadFile(filepath.Join(h.dir, wanrelease.ManifestName))
	signatureNow, signatureErr := os.ReadFile(filepath.Join(h.dir, wanrelease.SignatureName))
	if manifestErr != nil || signatureErr != nil || !bytes.Equal(manifestNow, h.manifestRaw) || !bytes.Equal(signatureNow, h.signatureRaw) {
		http.Error(w, "signed release changed; restart relay to re-verify", http.StatusServiceUnavailable)
		return
	}
	name := req.URL.Path
	w.Header().Set("X-Content-Type-Options", "nosniff")
	switch name {
	case wanrelease.ManifestName:
		http.ServeContent(w, req, name, time.Time{}, bytes.NewReader(h.manifestRaw))
		return
	case wanrelease.SignatureName:
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, req, name, time.Time{}, bytes.NewReader(h.signatureRaw))
		return
	}
	artifact, ok := h.artifacts[name]
	if !ok || filepath.Base(name) != name {
		http.NotFound(w, req)
		return
	}
	select {
	case h.verifySlots <- struct{}{}:
		defer func() { <-h.verifySlots }()
	default:
		http.Error(w, "release verification is busy", http.StatusServiceUnavailable)
		return
	}
	f, err := os.Open(filepath.Join(h.dir, name))
	if err != nil {
		http.Error(w, "signed artifact unavailable", http.StatusServiceUnavailable)
		return
	}
	var verified bytes.Buffer
	err = wanrelease.VerifyArtifact(f, &verified, artifact)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		http.Error(w, "signed artifact changed; restart relay to re-verify", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, req, name, time.Time{}, bytes.NewReader(verified.Bytes()))
}

func retiredRelayInstaller(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusGone)
	json.NewEncoder(w).Encode(map[string]string{
		"error": "the relay-hosted installer was retired because it cannot bootstrap trust; obtain the signed installer bundle from the independent release channel",
	})
}

func (r *Relay) handleSkills(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `inline; filename="SKILL.md"`)
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Write(skillMD)
}
