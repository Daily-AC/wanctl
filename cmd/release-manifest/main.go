// Command release-manifest creates and signs wanctl release manifests. Signing
// keys are accepted only through WANCTL_RELEASE_SIGNING_KEY and are never read
// from the source tree or written to disk.
package main

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"wanctl/internal/config"
	wanrelease "wanctl/internal/release"
)

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: release-manifest create VERSION DIST_DIR | sign DIST_DIR | verify DIST_DIR PUBLIC_KEY_FILE [RSA_PUBLIC_KEY_FILE] | public-key | public-key-pem | rsa-public-key-pem | rsa-public-key-xml [RSA_PUBLIC_KEY_FILE] | default-relay")
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
		// The RSA public key file is optional so that whoever publishes a release
		// can verify a downloaded CI artifact without holding either private key.
		if len(os.Args) != 4 && len(os.Args) != 5 {
			fatalf("usage: release-manifest verify DIST_DIR PUBLIC_KEY_FILE [RSA_PUBLIC_KEY_FILE]")
		}
		rsaPub := ""
		if len(os.Args) == 5 {
			rsaPub = os.Args[4]
		}
		err = verify(os.Args[2], os.Args[3], rsaPub)
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
	case "rsa-public-key-pem":
		if len(os.Args) != 2 {
			fatalf("usage: release-manifest rsa-public-key-pem")
		}
		var key *rsa.PrivateKey
		key, err = rsaSigningKey()
		if err == nil {
			var der []byte
			der, err = x509.MarshalPKIXPublicKey(&key.PublicKey)
			if err == nil {
				err = pem.Encode(os.Stdout, &pem.Block{Type: "PUBLIC KEY", Bytes: der})
			}
		}
	case "default-relay":
		// Single source for the relay baked into the installers, so they cannot
		// drift from the binary's own default.
		if len(os.Args) != 2 {
			fatalf("usage: release-manifest default-relay")
		}
		fmt.Println(config.EnvOr("WANCTL_RELAY", config.DefaultRelay))
	case "rsa-public-key-xml":
		// With a public key file, this renders the XML the Windows installer must
		// carry without needing the private key — that is how the publisher
		// checks a downloaded installer against the release's own public key.
		if len(os.Args) != 2 && len(os.Args) != 3 {
			fatalf("usage: release-manifest rsa-public-key-xml [RSA_PUBLIC_KEY_FILE]")
		}
		var pub *rsa.PublicKey
		if len(os.Args) == 3 {
			pub, err = rsaPublicKeyFromPEM(os.Args[2])
		} else {
			var key *rsa.PrivateKey
			if key, err = rsaSigningKey(); err == nil {
				pub = &key.PublicKey
			}
		}
		if err == nil {
			fmt.Println(rsaPublicKeyXML(pub))
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
	if err := os.WriteFile(filepath.Join(dir, wanrelease.SignatureName), sig, 0o644); err != nil {
		return err
	}

	// Second signature over the identical bytes, for the install scripts.
	rsaKey, err := rsaSigningKey()
	if err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	rsaSig, err := rsa.SignPKCS1v15(nil, rsaKey, crypto.SHA256, digest[:])
	if err != nil {
		return fmt.Errorf("rsa sign: %w", err)
	}
	if err := rsa.VerifyPKCS1v15(&rsaKey.PublicKey, crypto.SHA256, digest[:], rsaSig); err != nil {
		return errorsNew("rsa signing key is internally inconsistent")
	}
	return os.WriteFile(filepath.Join(dir, wanrelease.RSASignatureName), rsaSig, 0o644)
}

func verify(dir, publicKeyFile, rsaPublicKeyFile string) error {
	parsed, err := publicKeyFromPEM(publicKeyFile)
	if err != nil {
		return err
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

	// The scripts' signature covers the same bytes; a release where the two
	// disagree would install fine via `wanctl update` and fail for every new
	// user, so check both before publishing.
	var rsaPub *rsa.PublicKey
	if rsaPublicKeyFile != "" {
		rsaPub, err = rsaPublicKeyFromPEM(rsaPublicKeyFile)
	} else {
		var rsaKey *rsa.PrivateKey
		if rsaKey, err = rsaSigningKey(); err == nil {
			rsaPub = &rsaKey.PublicKey
		}
	}
	if err != nil {
		return err
	}
	rsaSigRaw, err := os.ReadFile(filepath.Join(dir, wanrelease.RSASignatureName))
	if err != nil {
		return fmt.Errorf("read script signature: %w", err)
	}
	digest := sha256.Sum256(manifestRaw)
	if err := rsa.VerifyPKCS1v15(rsaPub, crypto.SHA256, digest[:], rsaSigRaw); err != nil {
		return fmt.Errorf("script signature does not verify: %w", err)
	}
	fmt.Printf("verified signed release %s with key %s (+ %d-bit RSA script signature)\n",
		manifest.Version, wanrelease.KeyID(pub), rsaPub.N.BitLen())
	return nil
}

// publicKeyFromPEM reads exactly one PEM PUBLIC KEY block. Trailing data is
// rejected rather than ignored: a file with a second key would otherwise look
// verified while only its first key was ever checked.
func publicKeyFromPEM(path string) (any, error) {
	pemRaw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read public key: %w", err)
	}
	block, rest := pem.Decode(pemRaw)
	if block == nil || block.Type != "PUBLIC KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errorsNew("public key file is not a single PEM PUBLIC KEY block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	return parsed, nil
}

func rsaPublicKeyFromPEM(path string) (*rsa.PublicKey, error) {
	parsed, err := publicKeyFromPEM(path)
	if err != nil {
		return nil, err
	}
	pub, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, errorsNew("public key is not RSA")
	}
	if bits := pub.N.BitLen(); bits < 2048 {
		return nil, fmt.Errorf("RSA public key is %d bits; 2048 is the minimum", bits)
	}
	return pub, nil
}

// rsaSigningKey loads the script-facing signing key from WANCTL_RELEASE_RSA_KEY,
// base64-encoded PKCS#8 DER — same shape as WANCTL_RELEASE_SIGNING_KEY (single
// line, so GitLab can mask it), different algorithm.
func rsaSigningKey() (*rsa.PrivateKey, error) {
	encoded := strings.TrimSpace(os.Getenv("WANCTL_RELEASE_RSA_KEY"))
	if encoded == "" {
		return nil, errorsNew("WANCTL_RELEASE_RSA_KEY is required; the install scripts cannot verify a release without it")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errorsNew("WANCTL_RELEASE_RSA_KEY is not valid base64")
	}
	// Accept both DER encodings: `openssl genpkey -outform DER` emits PKCS#1 for
	// RSA, while other tooling emits PKCS#8. Rejecting one of them would mean
	// whoever generates the key has to know which flag produced it.
	var key *rsa.PrivateKey
	if parsed, pkcs8Err := x509.ParsePKCS8PrivateKey(raw); pkcs8Err == nil {
		rsaKey, ok := parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, errorsNew("WANCTL_RELEASE_RSA_KEY is not an RSA key")
		}
		key = rsaKey
	} else if parsed, pkcs1Err := x509.ParsePKCS1PrivateKey(raw); pkcs1Err == nil {
		key = parsed
	} else {
		return nil, fmt.Errorf("WANCTL_RELEASE_RSA_KEY is neither PKCS#8 nor PKCS#1 DER (pkcs8: %v; pkcs1: %v)", pkcs8Err, pkcs1Err)
	}
	if bits := key.N.BitLen(); bits < 2048 {
		return nil, fmt.Errorf("WANCTL_RELEASE_RSA_KEY is %d bits; 2048 is the minimum", bits)
	}
	return key, nil
}

// rsaPublicKeyXML renders an RSA public key in the .NET XML format, the only
// one PowerShell 5.1 can import without ImportSubjectPublicKeyInfo (which is
// .NET Core and up). Modulus is big-endian with leading zero bytes stripped,
// matching what RSACryptoServiceProvider.FromXmlString expects.
func rsaPublicKeyXML(pub *rsa.PublicKey) string {
	modulus := pub.N.Bytes()
	exponent := big.NewInt(int64(pub.E)).Bytes()
	return fmt.Sprintf("<RSAKeyValue><Modulus>%s</Modulus><Exponent>%s</Exponent></RSAKeyValue>",
		base64.StdEncoding.EncodeToString(modulus),
		base64.StdEncoding.EncodeToString(exponent))
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
