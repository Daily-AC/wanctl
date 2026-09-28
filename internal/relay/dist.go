package relay

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wanctl/internal/clientip"
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
	// Resolved once at startup rather than per request: the value cannot change
	// under a running relay, and a bad one should be reported on boot instead of
	// silently degrading every installer download.
	origin := installerOrigin()
	if origin == "" && os.Getenv("WANCTL_PUBLIC_ORIGIN") != "" {
		fmt.Fprintf(os.Stderr, "wanctl relay: WANCTL_PUBLIC_ORIGIN is not a plain http(s) origin; "+
			"served installers will keep pointing at the release page they were built for\n")
	}
	mux.HandleFunc("/install.sh", installerHandler(dir, "install.sh", "text/x-shellscript; charset=utf-8", origin))
	mux.HandleFunc("/install.ps1", installerHandler(dir, "install.ps1", "text/plain; charset=utf-8", origin))
	mux.HandleFunc("/skills", r.handleSkills)
}

// Each installer ships this marker for the serving relay to fill in. An
// installer built before the marker existed simply keeps its baked-in base.
const (
	shellRelaySelf      = `RELAY_SELF=""`
	powershellRelaySelf = `$relaySelf = ''`
)

// installerOrigin returns the origin to bake into served installers, or "" to
// serve them untouched.
//
// The value comes from WANCTL_PUBLIC_ORIGIN, never from the request: a Host or
// X-Forwarded-Proto header would let one poisoned cache entry redirect every
// subsequent install to an attacker's mirror. (That mirror could not forge a
// release — the installers verify the manifest against the RSA key they embed
// — but it could stall or downgrade installs.)
func installerOrigin() string {
	origin := strings.TrimRight(os.Getenv("WANCTL_PUBLIC_ORIGIN"), "/")
	if origin == "" {
		return ""
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	// This string is spliced into a shell script and a PowerShell script, so
	// only characters a URL genuinely needs may survive: a quote, backtick,
	// dollar sign, backslash or whitespace would stop being data and start
	// being code. A deployment sets this variable itself, so the guard is
	// against a typo becoming an execution bug, not against an outside attacker.
	if strings.ContainsAny(origin, "'\"`$\\ \t\r\n;|&<>()") {
		return ""
	}
	return origin
}

// localizeInstaller points an installer at the relay serving it.
func localizeInstaller(body []byte, name, origin string) []byte {
	if origin == "" {
		return body
	}
	switch name {
	case "install.sh":
		return bytes.Replace(body, []byte(shellRelaySelf), []byte(`RELAY_SELF="`+origin+`"`), 1)
	case "install.ps1":
		return bytes.Replace(body, []byte(powershellRelaySelf), []byte(`$relaySelf = '`+origin+`'`), 1)
	}
	return body
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
	// Every artifact is hashed against the signed manifest here, once, and
	// only a directory that passes whole is served at all.
	artifacts := make(map[string]verifiedArtifact)
	for _, artifact := range manifest.Artifacts {
		file, err := verifyArtifactFile(dir, artifact)
		if err != nil {
			return nil, fmt.Errorf("verify signed artifact %q: %w", artifact.Name, err)
		}
		artifacts[artifact.Name] = verifiedArtifact{Artifact: artifact, file: file}
	}
	// The install scripts verify this signature instead of the Ed25519 one, which
	// neither macOS's LibreSSL nor PowerShell 5.1 can check. Serving it is not a
	// trust decision — the scripts verify it against the key they embed — but
	// without it they abort before downloading anything. A release directory from
	// before v0.1.3 has no such file; distribution still works for `wanctl
	// update`, so warn rather than disable it.
	rsaSignatureRaw, err := os.ReadFile(filepath.Join(dir, wanrelease.RSASignatureName))
	if err != nil {
		fmt.Fprintf(os.Stderr, "wanctl relay: %s is missing; the one-line installers will not work: %v\n",
			wanrelease.RSASignatureName, err)
		rsaSignatureRaw = nil
	}
	return &signedDistHandler{
		dir: dir, manifestRaw: manifestRaw, signatureRaw: signatureRaw,
		rsaSignatureRaw: rsaSignatureRaw,
		artifacts:       artifacts,
	}, nil
}

// verifyArtifactFile hashes one artifact against its manifest entry and
// returns the identity of the file it hashed, taken from the same open file,
// so that what was checked is exactly what later requests are compared with.
func verifyArtifactFile(dir string, artifact wanrelease.Artifact) (os.FileInfo, error) {
	f, err := os.Open(filepath.Join(dir, artifact.Name))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err := wanrelease.VerifyArtifact(f, io.Discard, artifact); err != nil {
		return nil, err
	}
	return info, nil
}

type signedDistHandler struct {
	dir             string
	manifestRaw     []byte
	signatureRaw    []byte
	rsaSignatureRaw []byte
	artifacts       map[string]verifiedArtifact
	downloads       downloadSlots
}

// verifiedArtifact is a manifest entry and the file that was hashed against it
// when the relay started.
type verifiedArtifact struct {
	wanrelease.Artifact
	file os.FileInfo
}

// A download streams from the file, so each one costs a connection, a file
// handle and a few small buffers — never a copy of the artifact. These bound
// how many run at once, overall and per client. A download is held for as long
// as its reader takes (up to the server's write timeout), so a per-client share
// is what stops a few stalled readers from one address from taking every slot,
// the failure the old two-slot design had while it held its slots through the
// transfer (audit 2026-08-28, SEC-F-07). An installer or `wanctl update` fetches
// one artifact at a time; four leaves room for a retry beside it.
const (
	maxDownloads          = 64
	maxDownloadsPerClient = 4
)

// downloadSlots counts downloads in progress. The zero value is ready to use.
type downloadSlots struct {
	mu       sync.Mutex
	total    int
	byClient map[string]int
}

func (s *downloadSlots) acquire(client string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.total >= maxDownloads || s.byClient[client] >= maxDownloadsPerClient {
		return false
	}
	if s.byClient == nil {
		s.byClient = map[string]int{}
	}
	s.total++
	s.byClient[client]++
	return true
}

func (s *downloadSlots) release(client string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total--
	if s.byClient[client]--; s.byClient[client] <= 0 {
		delete(s.byClient, client)
	}
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
	if h.rsaSignatureRaw != nil {
		rsaNow, rsaErr := os.ReadFile(filepath.Join(h.dir, wanrelease.RSASignatureName))
		if rsaErr != nil || !bytes.Equal(rsaNow, h.rsaSignatureRaw) {
			http.Error(w, "signed release changed; restart relay to re-verify", http.StatusServiceUnavailable)
			return
		}
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
	case wanrelease.RSASignatureName:
		if h.rsaSignatureRaw == nil {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, req, name, time.Time{}, bytes.NewReader(h.rsaSignatureRaw))
		return
	}
	artifact, ok := h.artifacts[name]
	if !ok || filepath.Base(name) != name {
		http.NotFound(w, req)
		return
	}
	client := clientip.Key(req)
	if !h.downloads.acquire(client) {
		w.Header().Set("Retry-After", "30")
		http.Error(w, "too many downloads in progress; try again shortly", http.StatusServiceUnavailable)
		return
	}
	defer h.downloads.release(client)
	f, err := h.openVerified(artifact)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	// Straight from the file: Range works, and memory does not grow with the
	// artifact or with how slowly the client reads.
	http.ServeContent(w, req, name, time.Time{}, f)
}

var (
	errArtifactUnavailable = errors.New("signed artifact unavailable")
	errArtifactChanged     = errors.New("signed artifact changed; restart relay to re-verify")
)

// openVerified opens an artifact only if it is still the very file that was
// hashed at startup: same file, same size, same modification time. A
// deployment that replaces or rewrites the directory under a running relay
// then gets 503 rather than a mix of old and new, until a restart verifies the
// new release. This is a consistency check, not the trust decision — clients
// verify the signed manifest and its hashes themselves — which is why it no
// longer re-hashes the artifact on every request.
func (h *signedDistHandler) openVerified(artifact verifiedArtifact) (*os.File, error) {
	f, err := os.Open(filepath.Join(h.dir, artifact.Name))
	if err != nil {
		return nil, errArtifactUnavailable
	}
	info, err := f.Stat()
	if err != nil || !os.SameFile(info, artifact.file) || info.Size() != artifact.Size ||
		!info.ModTime().Equal(artifact.file.ModTime()) {
		f.Close()
		return nil, errArtifactChanged
	}
	return f, nil
}

// installerHandler serves the bootstrap installer that ships inside the signed
// release directory, rewritten to install from this relay's own /dl mirror.
// Users who won't visit the upstream release page have no other way to obtain
// it, so the relay serves it again — and for them a script that then downloads
// from that same unreachable page would be useless.
//
// TRUST LIMITATION: unlike /dl/*, this file is NOT covered by the signed
// manifest, and the public key it embeds is the one this same relay hands out.
// A compromised relay could therefore serve a malicious installer together with
// a matching key. Recipients should still prefer the independently hosted
// release page, and anyone can confirm what they downloaded out-of-band by
// comparing its SHA-256 against the checksum published with the release.
func installerHandler(dir, name, contentType, origin string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			http.Error(w, "installer unavailable", http.StatusServiceUnavailable)
			return
		}
		// Whoever fetched this script from this relay is, by construction,
		// someone for whom this relay is reachable — and often someone for whom
		// the upstream release page is not.
		body = localizeInstaller(body, name, origin)
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "public, max-age=300")
		http.ServeContent(w, req, name, time.Time{}, bytes.NewReader(body))
	}
}

func (r *Relay) handleSkills(w http.ResponseWriter, req *http.Request) {
	origin := strings.TrimRight(os.Getenv("WANCTL_PUBLIC_ORIGIN"), "/")
	if origin == "" {
		// Never turn an untrusted Host/X-Forwarded-Proto pair into controller
		// instructions. Behind a shared cache that let one request poison the
		// served SKILL.md with an attacker-controlled relay endpoint.
		http.Error(w, "WANCTL_PUBLIC_ORIGIN is required to serve /skills", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", `inline; filename="SKILL.md"`)
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Write(bytes.ReplaceAll(skillMD, []byte("@WANCTL_RELAY@"), []byte(origin)))
}
