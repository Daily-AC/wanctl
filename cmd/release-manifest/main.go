// Command release-manifest creates and signs wanctl release manifests. Signing
// keys are accepted only through WANCTL_RELEASE_SIGNING_KEY and are never read
// from the source tree or written to disk.
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	wanrelease "wanctl/internal/release"
)

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: release-manifest create VERSION DIST_DIR | sign DIST_DIR | verify DIST_DIR PUBLIC_KEY_FILE | public-key | public-key-pem")
	}
	var err error
	switch os.Args[1] {
	case "create":
		if len(os.Args) != 4 {
			fatalf("usage: release-manifest create VERSION DIST_DIR")
		}
		err = create(os.Args[2], os.Args[3])
	case "sign":
		if len(os.Args) != 3 {
			fatalf("usage: release-manifest sign DIST_DIR")
		}
		err = sign(os.Args[2])
	case "verify":
		if len(os.Args) != 4 {
			fatalf("usage: release-manifest verify DIST_DIR PUBLIC_KEY_FILE")
		}
		err = verify(os.Args[2], os.Args[3])
	case "public-key":
		if len(os.Args) != 2 {
			fatalf("usage: release-manifest public-key")
		}
		var key ed25519.PrivateKey
		key, err = signingKey()
		if err == nil {
			fmt.Println(base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)))
		}
	case "public-key-pem":
		if len(os.Args) != 2 {
			fatalf("usage: release-manifest public-key-pem")
		}
		var key ed25519.PrivateKey
		key, err = signingKey()
		if err == nil {
			// RFC 8410 SubjectPublicKeyInfo prefix for an Ed25519 raw public key.
			der := append([]byte{0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x03, 0x21, 0x00}, key.Public().(ed25519.PublicKey)...)
			err = pem.Encode(os.Stdout, &pem.Block{Type: "PUBLIC KEY", Bytes: der})
		}
	default:
		fatalf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fatalf("%v", err)
	}
}

func create(version, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var artifacts []wanrelease.Artifact
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "wanctl-") {
			continue
		}
		osName, arch, ok := platformFromName(entry.Name())
		if !ok {
			return fmt.Errorf("unrecognized release artifact %q", entry.Name())
		}
		path := filepath.Join(dir, entry.Name())
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		h := sha256.New()
		size, copyErr := io.Copy(h, io.LimitReader(f, wanrelease.MaxArtifactSize+1))
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if size <= 0 || size > wanrelease.MaxArtifactSize {
			return fmt.Errorf("artifact %q has invalid size %d", entry.Name(), size)
		}
		artifacts = append(artifacts, wanrelease.Artifact{OS: osName, Arch: arch, Name: entry.Name(), Size: size, SHA256: hex.EncodeToString(h.Sum(nil))})
	}
	if len(artifacts) == 0 {
		return errorsNew("no wanctl release artifacts found")
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Name < artifacts[j].Name })
	manifest := wanrelease.Manifest{Schema: 1, Version: version, PublishedAt: time.Now().UTC().Truncate(time.Second), Artifacts: artifacts}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if _, err := wanrelease.ParseManifest(raw); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, wanrelease.ManifestName), raw, 0o644)
}

func sign(dir string) error {
	key, err := signingKey()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(dir, wanrelease.ManifestName))
	if err != nil {
		return err
	}
	if _, err := wanrelease.ParseManifest(raw); err != nil {
		return err
	}
	sig := ed25519.Sign(key, raw)
	if !ed25519.Verify(key.Public().(ed25519.PublicKey), raw, sig) {
		return errorsNew("signing key is internally inconsistent")
	}
	return os.WriteFile(filepath.Join(dir, wanrelease.SignatureName), sig, 0o644)
}

func verify(dir, publicKeyFile string) error {
	pemRaw, err := os.ReadFile(publicKeyFile)
	if err != nil {
		return fmt.Errorf("read public key: %w", err)
	}
	block, rest := pem.Decode(pemRaw)
	if block == nil || block.Type != "PUBLIC KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return errorsNew("public key file is not a single PEM PUBLIC KEY block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse public key: %w", err)
	}
	pub, ok := parsed.(ed25519.PublicKey)
	if !ok || len(pub) != ed25519.PublicKeySize {
		return errorsNew("public key is not Ed25519")
	}
	manifestRaw, err := os.ReadFile(filepath.Join(dir, wanrelease.ManifestName))
	if err != nil {
		return err
	}
	signatureRaw, err := os.ReadFile(filepath.Join(dir, wanrelease.SignatureName))
	if err != nil {
		return err
	}
	trusted := base64.StdEncoding.EncodeToString(pub)
	manifest, err := wanrelease.VerifyManifest(manifestRaw, signatureRaw, trusted)
	if err != nil {
		return err
	}
	if err := wanrelease.VerifyDirectory(dir, manifestRaw, signatureRaw, trusted); err != nil {
		return err
	}
	fmt.Printf("verified signed release %s with key %s\n", manifest.Version, wanrelease.KeyID(pub))
	return nil
}

func signingKey() (ed25519.PrivateKey, error) {
	encoded := strings.TrimSpace(os.Getenv("WANCTL_RELEASE_SIGNING_KEY"))
	if encoded == "" {
		return nil, errorsNew("WANCTL_RELEASE_SIGNING_KEY is required; refusing to create an unsigned release")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errorsNew("WANCTL_RELEASE_SIGNING_KEY is not valid base64")
	}
	switch len(raw) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(raw), nil
	default:
		return nil, fmt.Errorf("WANCTL_RELEASE_SIGNING_KEY must decode to %d-byte seed or %d-byte private key", ed25519.SeedSize, ed25519.PrivateKeySize)
	}
}

func platformFromName(name string) (string, string, bool) {
	base := strings.TrimSuffix(name, ".exe")
	parts := strings.Split(base, "-")
	if len(parts) != 3 || parts[0] != "wanctl" || wanrelease.ArtifactName(parts[1], parts[2]) != name {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func errorsNew(message string) error { return fmt.Errorf("%s", message) }

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "release-manifest: "+format+"\n", args...)
	os.Exit(1)
}
