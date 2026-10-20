package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	wanrelease "wanctl/internal/release"
)

func TestCheckPromotion(t *testing.T) {
	script, err := filepath.Abs("scripts/check-promotion.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, change                  string
		noBeta, highestMismatch, pass bool
	}{
		{name: "identical commit", pass: true},
		{name: "changelog only", change: "internal/portal/changelog/v1.2.3.md", pass: true},
		{name: "code change", change: "main.go"},
		{name: "no beta", noBeta: true},
		{name: "highest beta is ten", highestMismatch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = dir
				cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+filepath.Join(dir, "no-global-config"), "GIT_CONFIG_NOSYSTEM=1")
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
				return strings.TrimSpace(string(out))
			}
			git("init", "-q")
			git("config", "user.name", "Release test")
			git("config", "user.email", "test@example.invalid")
			git("config", "commit.gpgsign", "false")
			git("config", "tag.gpgsign", "false")
			write := func(path string) {
				t.Helper()
				path = filepath.Join(dir, path)
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("change"), 0644); err != nil {
					t.Fatal(err)
				}
				git("add", ".")
				git("commit", "-qm", "change")
			}
			write("README.md")
			initial := git("rev-parse", "HEAD")
			if !tc.noBeta {
				git("tag", "v1.2.3-beta.9")
			}
			if tc.highestMismatch {
				write("main.go")
				git("tag", "-a", "v1.2.3-beta.10", "-m", "rehearsal")
				git("tag", "v1.2.3", initial)
			} else {
				if tc.change != "" {
					write(tc.change)
				}
				git("tag", "v1.2.3")
			}
			// Malformed suffixes must not count as the rehearsal.
			git("tag", "v1.2.3-beta.999.invalid")
			git("tag", "v1.2.3-beta.999-beta.1")
			cmd := exec.Command("sh", script, "v1.2.3")
			cmd.Dir = dir
			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.pass {
				t.Fatalf("pass=%v: %v\n%s", tc.pass, err, out)
			}
			if !tc.pass && !strings.Contains(string(out), "docs/releasing.md") {
				t.Fatalf("missing release instructions: %s", out)
			}
			if tc.highestMismatch && !strings.Contains(string(out), "v1.2.3-beta.10") {
				t.Fatalf("did not compare beta.10: %s", out)
			}
		})
	}
	// Betas need only a valid spelling, not a repository or a pre-existing tag.
	for _, tag := range []string{"v1.2.3-beta.1", "v1.2.3-beta.10"} {
		cmd := exec.Command("sh", script, tag)
		cmd.Dir = t.TempDir()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", tag, err, out)
		}
	}
	for _, suffix := range []string{"-beta", "-beta.0", "-beta.01", "-rc.1", "-alpha.1", "-Beta.1", "+meta", "-beta.1+meta", "-beta.1.2"} {
		cmd := exec.Command("sh", script, "v1.2.3"+suffix)
		cmd.Dir = t.TempDir()
		if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "docs/releasing.md") {
			t.Fatalf("%s: %v\n%s", suffix, err, out)
		}
	}
}

// Compilation is replaced by tiny payloads, but packaging, manifest creation,
// both signatures and validation run the real scripts and manifest tool. No
// production key, Android SDK, GitHub write or multi-platform compiler is used.
func TestReleaseScriptsBetaAndStable(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the manifest tool")
	}
	root := t.TempDir()
	write := func(path string, data []byte, mode os.FileMode) {
		t.Helper()
		path = filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"build-release.sh", "validate-release.sh", "release-targets.sh", "release-version.sh", "install.sh.in", "install.ps1.in", "publish-release.sh", "check-promotion.sh"} {
		data, err := os.ReadFile(filepath.Join("scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		write(filepath.Join("scripts", name), data, 0755)
	}
	tool := filepath.Join(root, "release-manifest")
	if out, err := exec.Command("go", "build", "-o", tool, "./cmd/release-manifest").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	write("bin/go", []byte(`#!/bin/sh
set -eu
if [ "$1" = run ] && [ "$2" = ./cmd/release-manifest ]; then
 shift 2
 exec "$WANCTL_TEST_MANIFEST_TOOL" "$@"
fi
if [ "$1" = build ]; then
 while [ "$1" != -o ]; do shift; done
 printf '%s/%s test binary\n' "$GOOS" "$GOARCH" > "$2"
 exit 0
fi
exit 1
`), 0755)
	write("scripts/build-apk.sh", []byte(`#!/bin/sh
set -eu
touch apk-built
mkdir -p build/android
for arch in arm64 arm 386 amd64; do
 printf '%s APK\n' "$arch" > "build/android/wanctl-android-$arch.apk"
done
`), 0755)
	// Stable's NDK lookup still runs even though the compiler is a test stub.
	host := "linux-x86_64"
	if runtime.GOOS == "darwin" {
		host = "darwin-x86_64"
	}
	for _, triple := range []string{"armv7a-linux-androideabi", "i686-linux-android", "x86_64-linux-android"} {
		write("ndk/toolchains/llvm/prebuilt/"+host+"/bin/"+triple+"29-clang", []byte("#!/bin/sh\nexit 1\n"), 0755)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WANCTL_RELEASE_SIGNING_KEY", base64.StdEncoding.EncodeToString(priv.Seed()))
	t.Setenv("WANCTL_RELEASE_RSA_KEY", base64.StdEncoding.EncodeToString(x509.MarshalPKCS1PrivateKey(rsaKey)))
	t.Setenv("WANCTL_RELEASE_PREVIOUS_PUBLIC_KEYS", "")
	t.Setenv("WANCTL_RELEASE_BASE", "https://example.invalid/releases/latest/download")
	t.Setenv("WANCTL_SKIP_APK", "")
	t.Setenv("WANCTL_TEST_MANIFEST_TOOL", tool)
	t.Setenv("ANDROID_NDK_HOME", filepath.Join(root, "ndk"))
	t.Setenv("PATH", filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	run := func(script string, args ...string) (string, error) {
		t.Helper()
		cmd := exec.Command("sh", append([]string{filepath.Join(root, "scripts", script)}, args...)...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	must := func(script string, args ...string) {
		t.Helper()
		if out, err := run(script, args...); err != nil {
			t.Fatalf("%s %v: %v\n%s", script, args, err, out)
		}
	}
	manifest := func() wanrelease.Manifest {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(root, "release/manifest.json"))
		if err != nil {
			t.Fatal(err)
		}
		m, err := wanrelease.ParseManifest(b)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	// Staged APKs from an earlier stable must not leak into the beta manifest.
	write("build/android/wanctl-android-arm64.apk", []byte("stale APK"), 0644)
	must("build-release.sh", "v1.2.3-beta.1")
	if _, err := os.Stat(filepath.Join(root, "apk-built")); !os.IsNotExist(err) {
		t.Fatal("beta invoked APK builder")
	}
	for _, a := range manifest().Artifacts {
		if a.OS == "android" || strings.HasSuffix(a.Name, ".apk") {
			t.Fatalf("beta artifact: %+v", a)
		}
	}
	must("validate-release.sh", "v1.2.3-beta.1", "release")
	write("release/wanctl-android-arm64.apk", []byte("unexpected APK"), 0644)
	if out, err := run("validate-release.sh", "v1.2.3-beta.1", "release"); err == nil || !strings.Contains(out, "unexpected files") {
		t.Fatalf("beta accepted APK: %v %s", err, out)
	}
	must("build-release.sh", "v1.2.3")
	apks := 0
	for _, a := range manifest().Artifacts {
		if strings.HasSuffix(a.Name, ".apk") {
			apks++
		}
	}
	if apks != 4 {
		t.Fatalf("stable APK count = %d", apks)
	}
	if out, err := run("validate-release.sh", "v1.2.3", "release"); err == nil || !strings.Contains(out, "no changelog entry") {
		t.Fatalf("stable needs changelog: %v %s", err, out)
	}
	write("internal/portal/changelog/v1.2.3.md", []byte("Stable notes"), 0644)
	must("validate-release.sh", "v1.2.3", "release")
	// Exercise the manual publisher with a local git repo and a recording gh;
	// its beta path must not accidentally mark a prerelease as latest either.
	for _, args := range [][]string{
		{"init", "-q"}, {"config", "user.name", "Release test"}, {"config", "user.email", "test@example.invalid"},
		{"-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "candidate"},
		{"-c", "tag.gpgsign=false", "tag", "v1.2.3-beta.1"}, {"-c", "tag.gpgsign=false", "tag", "v1.2.3"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	write("bin/gh", []byte(`#!/bin/sh
set -eu
if [ "$2" = view ]; then exit 1; fi
printf '%s\n' "$@" > "$WANCTL_TEST_GH_ARGS"
`), 0755)
	capture := filepath.Join(root, "gh-args")
	t.Setenv("WANCTL_TEST_GH_ARGS", capture)
	for _, tag := range []string{"v1.2.3", "v1.2.3-beta.1"} {
		must("build-release.sh", tag)
		must("publish-release.sh", tag, "release")
		out, err := os.ReadFile(capture)
		if err != nil {
			t.Fatal(err)
		}
		beta := strings.Contains(tag, "-beta.")
		for _, flag := range []string{"--prerelease", "--latest=false", "--generate-notes"} {
			if strings.Contains(string(out), flag+"\n") != beta {
				t.Fatalf("%s: %s", tag, out)
			}
		}
		if strings.Contains(string(out), "--latest\n") == beta {
			t.Fatalf("%s: %s", tag, out)
		}
	}
	must("build-release.sh", "v1.2.3")
	if err := os.Remove(filepath.Join(root, "release/wanctl-android-arm64.apk")); err != nil {
		t.Fatal(err)
	}
	if out, err := run("validate-release.sh", "v1.2.3", "release"); err == nil || !strings.Contains(out, "missing or unexpected files") {
		t.Fatalf("stable accepted missing APK: %v %s", err, out)
	}
	for _, script := range []string{"build-release.sh", "validate-release.sh", "publish-release.sh"} {
		for _, suffix := range []string{"-beta", "-beta.0", "-beta.01", "-rc.1", "-alpha.1", "-Beta.1", "+meta", "-beta.1+meta", "-beta.1.2"} {
			if out, err := run(script, "v1.2.3"+suffix, "release", "notes.md"); err == nil || !strings.Contains(out, "invalid release version") {
				t.Fatalf("%s accepted %s: %v %s", script, suffix, err, out)
			}
		}
	}
}

func TestAPKBuilderRejectsBeta(t *testing.T) {
	out, err := exec.Command("sh", "scripts/build-apk.sh", "v1.2.3-beta.1").CombinedOutput()
	if err == nil || !strings.Contains(string(out), "beta releases do not ship APKs") {
		t.Fatalf("%v: %s", err, out)
	}
}
