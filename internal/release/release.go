// Package release verifies wanctl release manifests against compile-time trust
// anchors. The relay is deliberately not part of this trust decision.
package release

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	ManifestName    = "manifest.json"
	SignatureName   = "manifest.json.sig"
	MaxManifestSize = 1 << 20
	MaxArtifactSize = 64 << 20

	// RSASignatureName is a second signature over the exact same manifest bytes,
	// for the install scripts only. `wanctl update` verifies SignatureName with
	// the Ed25519 key compiled into the binary and never looks at this one.
	//
	// The scripts need their own algorithm because Ed25519 verification is not
	// reachable from a stock shell on two of the three platforms we ship to:
	// macOS ships LibreSSL, whose pkeyutl has no -rawin, and Windows PowerShell
	// has no Ed25519 at all (installing OpenSSL to get it means a multi-minute
	// download from a slow origin, when it succeeds). RSA verifies natively in
	// both: `openssl dgst -verify` on LibreSSL, RSACryptoServiceProvider on
	// PowerShell 5.1.
	RSASignatureName = "manifest.json.rsa.sig"
)

// TrustedPublicKeys is set by the release build with -ldflags -X. It is a
// comma-separated list of base64-encoded raw Ed25519 public keys. Keeping more
// than one key enables overlap during key rotation. An empty value fails closed.
var TrustedPublicKeys string

type Manifest struct {
	Schema      int        `json:"schema"`
	Version     string     `json:"version"`
	PublishedAt time.Time  `json:"published_at"`
	Artifacts   []Artifact `json:"artifacts"`
}

type Artifact struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func ParseTrustedKeys(encoded string) (map[string]ed25519.PublicKey, error) {
	keys := make(map[string]ed25519.PublicKey)
	for _, value := range strings.Split(encoded, ",") {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(value)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("invalid release public key")
		}
		pub := ed25519.PublicKey(raw)
		keys[KeyID(pub)] = pub
	}
	if len(keys) == 0 {
		return nil, errors.New("no trusted release public key compiled into this binary")
	}
	return keys, nil
}

func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

func ParseManifest(raw []byte) (Manifest, error) {
	if len(raw) == 0 || len(raw) > MaxManifestSize {
		return Manifest{}, fmt.Errorf("manifest size %d is invalid", len(raw))
	}
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return Manifest{}, err
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if err := requireEOF(dec); err != nil {
		return Manifest{}, err
	}
	if m.Schema != 1 {
		return Manifest{}, fmt.Errorf("unsupported manifest schema %d", m.Schema)
	}
	if _, err := parseVersion(m.Version); err != nil {
		return Manifest{}, fmt.Errorf("invalid release version: %w", err)
	}
	if m.PublishedAt.IsZero() || len(m.Artifacts) == 0 {
		return Manifest{}, errors.New("manifest is missing publication time or artifacts")
	}
	seen := make(map[string]bool)
	for _, a := range m.Artifacts {
		if a.OS == "" || a.Arch == "" || a.Name == "" || filepath.Base(a.Name) != a.Name {
			return Manifest{}, errors.New("manifest contains an invalid artifact identity")
		}
		if a.Name != ArtifactName(a.OS, a.Arch) {
			return Manifest{}, fmt.Errorf("artifact name %q does not match %s/%s", a.Name, a.OS, a.Arch)
		}
		if a.Size <= 0 || a.Size > MaxArtifactSize {
			return Manifest{}, fmt.Errorf("artifact %q size %d is invalid", a.Name, a.Size)
		}
		decoded, err := hex.DecodeString(a.SHA256)
		if err != nil || len(decoded) != sha256.Size || a.SHA256 != strings.ToLower(a.SHA256) {
			return Manifest{}, fmt.Errorf("artifact %q has invalid sha256", a.Name)
		}
		key := a.OS + "/" + a.Arch
		if seen[key] {
			return Manifest{}, fmt.Errorf("duplicate artifact platform %s", key)
		}
		seen[key] = true
	}
	return m, nil
}

func rejectDuplicateJSONKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	first, err := dec.Token()
	if err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}
	if err := walkJSONValue(dec, first); err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("signed JSON contains trailing data")
	}
	return nil
}

func walkJSONValue(dec *json.Decoder, token json.Token) error {
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for dec.More() {
			keyToken, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if seen[key] {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = true
			value, err := dec.Token()
			if err != nil {
				return err
			}
			if err := walkJSONValue(dec, value); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("unterminated JSON object")
		}
	case '[':
		for dec.More() {
			value, err := dec.Token()
			if err != nil {
				return err
			}
			if err := walkJSONValue(dec, value); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("unterminated JSON array")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
	return nil
}

func VerifyManifest(manifestRaw, signatureRaw []byte, trusted string) (Manifest, error) {
	m, err := ParseManifest(manifestRaw)
	if err != nil {
		return Manifest{}, err
	}
	keys, err := ParseTrustedKeys(trusted)
	if err != nil {
		return Manifest{}, err
	}
	if len(signatureRaw) != ed25519.SignatureSize {
		return Manifest{}, errors.New("release signature is missing or has an invalid size")
	}
	for _, pub := range keys {
		if ed25519.Verify(pub, manifestRaw, signatureRaw) {
			return m, nil
		}
	}
	return Manifest{}, errors.New("release manifest signature verification failed")
}

// APKArch is the pseudo-architecture under which the Android APK for a GOARCH
// rides in the release manifest: "arm64.apk", "arm.apk", "386.apk", "amd64.apk".
//
// The APK is a second artifact for one real platform, which the schema has no
// field for — and bumping the schema would strand every already-installed
// client, since VerifyManifest rejects a version it does not know and that is
// the code path `wanctl update` runs before it can update itself. Naming the
// arch "<goarch>.apk" fits the existing rule (Name == "wanctl-"+OS+"-"+Arch, so
// the file is wanctl-android-arm64.apk), keeps the OS/Arch pair unique against
// the real android/<goarch> entry, and is invisible to older clients: no Go
// toolchain reports GOARCH as "arm64.apk", so Select never matches it for them.
//
// One APK per ABI rather than a universal one: the binary inside knows its
// own runtime.GOARCH and fetches the matching APK, so an in-app update
// downloads one binary's worth, not four.
func APKArch(goarch string) string { return goarch + ".apk" }

// AndroidAPKArch is the arm64 APK's pseudo-architecture — the original and
// only one before v0.3.1, which every app installed before then asks for.
const AndroidAPKArch = "arm64.apk"

// ErrUpToDate reports that the signed manifest offers nothing newer than what
// is installed. It is a normal outcome, not a failure, and callers that present
// it to a user need to tell the two apart.
var ErrUpToDate = errors.New("already up to date")

func Select(m Manifest, goos, goarch, currentVersion string) (Artifact, error) {
	if currentVersion != "" && currentVersion != "dev" {
		current, err := parseVersion(currentVersion)
		if err != nil {
			return Artifact{}, fmt.Errorf("invalid current build version %q: %w", currentVersion, err)
		}
		next, _ := parseVersion(m.Version)
		if compareVersion(next, current) <= 0 {
			return Artifact{}, fmt.Errorf("%w: release %s is not newer than installed %s", ErrUpToDate, m.Version, currentVersion)
		}
	}
	for _, a := range m.Artifacts {
		if a.OS == goos && a.Arch == goarch {
			return a, nil
		}
	}
	return Artifact{}, fmt.Errorf("signed release %s has no artifact for %s/%s", m.Version, goos, goarch)
}

func VerifyArtifact(r io.Reader, dst io.Writer, artifact Artifact) error {
	h := sha256.New()
	written, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(r, artifact.Size+1))
	if err != nil {
		return fmt.Errorf("read artifact: %w", err)
	}
	if written != artifact.Size {
		return fmt.Errorf("artifact size mismatch: got %d, want %d", written, artifact.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != artifact.SHA256 {
		return fmt.Errorf("artifact sha256 mismatch: got %s", got)
	}
	return nil
}

func VerifyDirectory(dir string, manifestRaw, signatureRaw []byte, trusted string) error {
	m, err := VerifyManifest(manifestRaw, signatureRaw, trusted)
	if err != nil {
		return err
	}
	for _, a := range m.Artifacts {
		f, err := os.Open(filepath.Join(dir, a.Name))
		if err != nil {
			return fmt.Errorf("open signed artifact %q: %w", a.Name, err)
		}
		err = VerifyArtifact(f, io.Discard, a)
		closeErr := f.Close()
		if err != nil {
			return fmt.Errorf("verify signed artifact %q: %w", a.Name, err)
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func ArtifactName(goos, goarch string) string {
	name := "wanctl-" + goos + "-" + goarch
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

func requireEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("signed JSON contains trailing data")
	}
	return nil
}

type version [3]uint64

func parseVersion(value string) (version, error) {
	value = strings.TrimPrefix(value, "v")
	if strings.ContainsAny(value, "+-") {
		return version{}, errors.New("pre-release/build metadata is not allowed")
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return version{}, errors.New("expected vMAJOR.MINOR.PATCH")
	}
	var v version
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return version{}, errors.New("invalid numeric version component")
		}
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return version{}, errors.New("invalid numeric version component")
		}
		v[i] = n
	}
	return v, nil
}

func compareVersion(a, b version) int {
	for i := range a {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}
