package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	wanrelease "wanctl/internal/release"
)

// TestAndroidAPKRidesInTheExistingSchema is the load-bearing claim of the APK
// distribution design: the APK is added to the signed manifest without a schema
// change, so a client built before the APK existed still parses the manifest it
// needs in order to update itself.
//
// If this breaks, the failure is not "Android cannot update" — it is that every
// already-deployed wanctl on every platform stops being able to update at all,
// because VerifyManifest rejects the whole document and `wanctl update` verifies
// before it does anything else.
func TestAndroidAPKRidesInTheExistingSchema(t *testing.T) {
	name := wanrelease.ArtifactName("android", wanrelease.AndroidAPKArch)
	if name != "wanctl-android-arm64.apk" {
		t.Fatalf("ArtifactName(android, %s) = %q", wanrelease.AndroidAPKArch, name)
	}

	payload := []byte("not really an APK")
	sum := sha256.Sum256(payload)
	m := wanrelease.Manifest{
		Schema:      1,
		Version:     "v9.9.9",
		PublishedAt: time.Now().UTC(),
		Artifacts: []wanrelease.Artifact{
			{
				OS: "android", Arch: "arm64",
				Name:   wanrelease.ArtifactName("android", "arm64"),
				Size:   int64(len(payload)),
				SHA256: hex.EncodeToString(sum[:]),
			},
			{
				OS: "android", Arch: wanrelease.AndroidAPKArch,
				Name:   name,
				Size:   int64(len(payload)),
				SHA256: hex.EncodeToString(sum[:]),
			},
		},
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := wanrelease.ParseManifest(raw)
	if err != nil {
		t.Fatalf("a manifest carrying the APK must still parse: %v", err)
	}
	if len(parsed.Artifacts) != 2 {
		t.Fatalf("artifacts = %d, want 2", len(parsed.Artifacts))
	}

	// The APK entry must be invisible to a normal client: no Go toolchain
	// reports GOARCH as "arm64.apk", so an Android device selecting its own
	// platform still gets the bare binary.
	got, err := wanrelease.Select(parsed, "android", "arm64", "v0.0.1")
	if err != nil {
		t.Fatalf("Select(android/arm64): %v", err)
	}
	if got.Name != "wanctl-android-arm64" {
		t.Fatalf("android/arm64 selected %q", got.Name)
	}
	apk, err := wanrelease.Select(parsed, "android", wanrelease.AndroidAPKArch, "v0.0.1")
	if err != nil {
		t.Fatalf("Select(android/%s): %v", wanrelease.AndroidAPKArch, err)
	}
	if apk.Name != name {
		t.Fatalf("APK selected %q", apk.Name)
	}
}

// TestUpToDateIsDistinguishable covers the Android app's "已是最新" path: it has
// to tell "nothing newer" apart from "the download failed", and string matching
// on a message that happens to be Chinese today is not a contract.
func TestUpToDateIsDistinguishable(t *testing.T) {
	m := wanrelease.Manifest{Version: "v1.0.0"}
	_, err := wanrelease.Select(m, "android", wanrelease.AndroidAPKArch, "v1.0.0")
	if !errors.Is(err, wanrelease.ErrUpToDate) {
		t.Fatalf("same version: err = %v, want ErrUpToDate", err)
	}
	_, err = wanrelease.Select(m, "android", wanrelease.AndroidAPKArch, "v2.0.0")
	if !errors.Is(err, wanrelease.ErrUpToDate) {
		t.Fatalf("older release: err = %v, want ErrUpToDate", err)
	}
	// A missing platform is a real failure and must not be swallowed as "current".
	_, err = wanrelease.Select(m, "plan9", "arm64", "v0.0.1")
	if errors.Is(err, wanrelease.ErrUpToDate) {
		t.Fatalf("missing artifact reported as up to date: %v", err)
	}
}

// TestRunningFromAPK pins the shape that decides whether `wanctl update` is
// allowed to touch its own file. Getting this wrong in the permissive direction
// is what produced the v0.1.7-era bug where update aimed at /system/bin/linker64
// — on a rooted device, at the system's dynamic linker.
func TestRunningFromAPK(t *testing.T) {
	for _, tc := range []struct {
		path string
		want bool
	}{
		{"/data/app/~~abc==/dev.wanctl.agent-xyz==/lib/arm64/libwanctl.so", true},
		{"/data/app/dev.wanctl.agent-1/lib/arm64/libwanctl.so", true},
		// Not an APK: the adb-push location, Termux, and a plain copy.
		{"/data/local/tmp/wanctl", false},
		{"/data/data/com.termux/files/usr/bin/wanctl", false},
		{"/data/app/~~abc==/dev.wanctl.agent-xyz==/wanctl", false},
		{"/usr/local/bin/wanctl", false},
	} {
		if got := isAPKPath(tc.path); got != tc.want {
			t.Errorf("isAPKPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
