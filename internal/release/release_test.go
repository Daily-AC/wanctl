package release

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func signedFixture(t *testing.T, version, goos, goarch string, payload []byte) ([]byte, []byte, string, Artifact) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(payload)
	a := Artifact{OS: goos, Arch: goarch, Name: ArtifactName(goos, goarch), Size: int64(len(payload)), SHA256: hex.EncodeToString(h[:])}
	m := Manifest{Schema: 1, Version: version, PublishedAt: time.Now().UTC(), Artifacts: []Artifact{a}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(priv, raw)
	return raw, sig, base64.StdEncoding.EncodeToString(pub), a
}

func TestVerifySignedRelease(t *testing.T) {
	manifest, sig, trusted, artifact := signedFixture(t, "v1.2.4", "linux", "amd64", []byte("binary"))
	m, err := VerifyManifest(manifest, sig, trusted)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Select(m, "linux", "amd64", "v1.2.3")
	if err != nil || got != artifact {
		t.Fatalf("Select() = %#v, %v", got, err)
	}
	if err := VerifyArtifact(strings.NewReader("binary"), &bytes.Buffer{}, got); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRejectsTamperingUnsignedWrongPlatformReplayAndOversize(t *testing.T) {
	manifest, sig, trusted, artifact := signedFixture(t, "v2.0.0", "linux", "amd64", []byte("binary"))

	tampered := append([]byte(nil), manifest...)
	tampered[len(tampered)-1] ^= 1
	if _, err := VerifyManifest(tampered, sig, trusted); err == nil {
		t.Fatal("tampered manifest accepted")
	}
	if _, err := VerifyManifest(manifest, nil, trusted); err == nil {
		t.Fatal("unsigned manifest accepted")
	}
	m, err := VerifyManifest(manifest, sig, trusted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Select(m, "darwin", "arm64", "v1.0.0"); err == nil {
		t.Fatal("wrong platform accepted")
	}
	if _, err := Select(m, "linux", "amd64", "v2.0.0"); err == nil {
		t.Fatal("replayed release accepted")
	}
	if _, err := Select(m, "linux", "amd64", "v3.0.0"); err == nil {
		t.Fatal("downgrade accepted")
	}
	if err := VerifyArtifact(strings.NewReader("binary-tampered"), &bytes.Buffer{}, artifact); err == nil {
		t.Fatal("oversized artifact accepted")
	}
}

func TestManifestRejectsOversizeAndUnknownFields(t *testing.T) {
	if _, err := ParseManifest(bytes.Repeat([]byte{'x'}, MaxManifestSize+1)); err == nil {
		t.Fatal("oversized manifest accepted")
	}
	raw := `{"schema":1,"version":"v1.0.0","published_at":"2026-01-01T00:00:00Z","artifacts":[],"surprise":true}`
	if _, err := ParseManifest([]byte(raw)); err == nil {
		t.Fatal("unknown manifest field accepted")
	}
	for _, duplicate := range []string{
		`{"schema":1,"schema":1,"version":"v1.0.0","published_at":"2026-01-01T00:00:00Z","artifacts":[]}`,
		`{"schema":1,"version":"v1.0.0","published_at":"2026-01-01T00:00:00Z","artifacts":[{"os":"linux","os":"darwin","arch":"amd64","name":"wanctl-linux-amd64","size":1,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}`,
	} {
		if _, err := ParseManifest([]byte(duplicate)); err == nil {
			t.Fatal("duplicate manifest field accepted")
		}
	}
}

func TestVersionGrammar(t *testing.T) {
	for _, v := range []string{"v0.26.0", "v0.26.0-beta.1", "v0.26.0-beta.10", "v0.26.0-beta.18446744073709551616"} {
		raw, _, _, _ := signedFixture(t, v, "linux", "amd64", []byte("binary"))
		if _, err := ParseManifest(raw); err != nil {
			t.Errorf("%s: %v", v, err)
		}
	}
	for _, suffix := range []string{"-beta", "-beta.0", "-beta.01", "-rc.1", "-alpha.1", "-Beta.1", "+meta", "-beta.1+meta", "-beta.1.2"} {
		raw, _, _, _ := signedFixture(t, "v0.26.0"+suffix, "linux", "amd64", []byte("binary"))
		if _, err := ParseManifest(raw); err == nil {
			t.Errorf("accepted %s", suffix)
		}
	}
	for _, v := range []string{"0.26.0", "v01.2.3", "v1.02.3", "v1.2.03", "v1.2", "v1.2.3\n"} {
		if err := ValidateVersion(v); err == nil {
			t.Errorf("accepted %q", v)
		}
	}
}

func TestVersionOrdering(t *testing.T) {
	versions := []string{"v0.25.9", "v0.26.0-beta.1", "v0.26.0-beta.2", "v0.26.0-beta.10", "v0.26.0", "v1.0.0"}
	for i, a := range versions {
		for j, b := range versions {
			got, err := CompareVersions(a, b)
			if err != nil || (i < j && got >= 0) || (i == j && got != 0) || (i > j && got <= 0) {
				t.Errorf("compare %s, %s = %d, %v", a, b, got, err)
			}
		}
	}
}

func TestSelectReleaseChannel(t *testing.T) {
	for _, current := range []string{"", "dev", "v0.25.9", "v0.26.0", "v0.26.0-beta.1", "v0.26.0-beta.2", "v0.26.0-beta.10"} {
		for _, next := range []string{"v0.25.9", "v0.26.0-beta.1", "v0.26.0-beta.2", "v0.26.0-beta.10", "v0.26.0"} {
			raw, _, _, _ := signedFixture(t, next, "linux", "amd64", []byte("binary"))
			m, err := ParseManifest(raw)
			if err != nil {
				t.Fatal(err)
			}
			_, err = Select(m, "linux", "amd64", current)
			var want error
			if strings.Contains(next, "-beta.") && !strings.Contains(current, "-beta.") {
				want = ErrBetaNotAllowed
			} else if current != "" && current != "dev" {
				cmp, _ := CompareVersions(next, current)
				if cmp <= 0 {
					want = ErrUpToDate
				}
			}
			if !errors.Is(err, want) {
				t.Errorf("%s -> %s: %v, want %v", current, next, err, want)
			}
		}
	}
}
