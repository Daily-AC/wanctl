package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	wanrelease "wanctl/internal/release"
)

func TestUpdateVersionSources(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", "https://relay.example")
	for _, tc := range []struct {
		base string
		want []string
	}{
		{"https://github.com/Daily-AC/wanctl/releases/latest/download/", []string{"https://github.com/Daily-AC/wanctl/releases/download/v0.26.0-beta.2", "https://relay.example/dl"}},
		{"https://custom.example/beta/", []string{"https://custom.example/beta", "https://relay.example/dl"}},
		{"", []string{"https://relay.example/dl"}},
		{"https://relay.example/dl", []string{"https://relay.example/dl"}},
	} {
		t.Setenv("WANCTL_RELEASE_BASE", tc.base)
		got, err := updateSourcesForVersion("v0.26.0-beta.2")
		if err != nil || !slices.Equal(got, tc.want) {
			t.Fatalf("%s: %v, %v", tc.base, got, err)
		}
	}
}

func TestExplicitUpdateVerificationAndFallback(t *testing.T) {
	const requested = "v0.26.0-beta.1"
	wrong := signedUpdateServer(t, []byte("wrong"), "linux", "amd64", "v0.26.0", nil)
	defer wrong.Close()
	wrongKey := wanrelease.TrustedPublicKeys
	right := signedUpdateServer(t, []byte("requested binary"), "linux", "amd64", requested, nil)
	defer right.Close()
	wanrelease.TrustedPublicKeys += "," + wrongKey
	bases := []string{wrong.URL + "/dl", right.URL + "/dl"}
	got, err := fetchUpdateVersion(t.Context(), bases, t.TempDir(), "linux", "amd64", "v0.27.0", requested)
	if err != nil || got.version != requested {
		t.Fatalf("%+v, %v", got, err)
	}
	b, err := os.ReadFile(got.path)
	if err != nil || string(b) != "requested binary" {
		t.Fatalf("%q, %v", b, err)
	}
	_, err = fetchUpdateVersion(t.Context(), bases, t.TempDir(), "linux", "amd64", "dev", "v0.24.0")
	for _, want := range append(bases, "v0.24.0", "签名清单版本", "不一致") {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q: %v", want, err)
		}
	}
	for _, part := range []string{wanrelease.SignatureName, "wanctl-linux-amd64"} {
		t.Run(part, func(t *testing.T) {
			srv := signedUpdateServer(t, []byte("payload"), "linux", "amd64", "v0.26.0", func(path string, b []byte) []byte {
				if strings.HasSuffix(path, part) {
					return []byte("tampered")
				}
				return b
			})
			defer srv.Close()
			dir := t.TempDir()
			_, _, err := downloadSignedVersion(t.Context(), srv.URL+"/dl", dir, "linux", "amd64", "dev", requested)
			if err == nil {
				t.Fatal("accepted tampered or wrong version")
			}
			if part == wanrelease.SignatureName && strings.Contains(err.Error(), "不一致") {
				t.Fatalf("compared version before verifying signature: %v", err)
			}
			// Also exercise the matching-version path: artifact tampering must fail.
			_, _, err = downloadSignedVersion(t.Context(), srv.URL+"/dl", dir, "linux", "amd64", "dev", "v0.26.0")
			if err == nil {
				t.Fatal("accepted tampered bytes")
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Fatal("left download behind")
			}
		})
	}
}

func TestBetaSourceFallsBackForStable(t *testing.T) {
	beta := signedUpdateServer(t, []byte("beta"), "linux", "amd64", "v0.26.0-beta.1", nil)
	defer beta.Close()
	betaKey := wanrelease.TrustedPublicKeys
	stable := signedUpdateServer(t, []byte("stable"), "linux", "amd64", "v0.25.1", nil)
	defer stable.Close()
	wanrelease.TrustedPublicKeys += "," + betaKey
	for _, current := range []string{"v0.25.0", "dev", ""} {
		got, err := overSources([]string{beta.URL + "/dl", stable.URL + "/dl"}, func(base string) (updateFetch, error) {
			v, err := checkSignedUpdate(t.Context(), base, "linux", "amd64", current)
			if base == beta.URL+"/dl" && (!errors.Is(err, wanrelease.ErrBetaNotAllowed) || errors.Is(err, wanrelease.ErrUpToDate)) {
				t.Fatalf("beta refusal = %v", err)
			}
			return updateFetch{version: v}, err
		})
		if err != nil || got.version != "v0.25.1" {
			t.Fatalf("%+v, %v", got, err)
		}
	}
}

func TestExplicitUpdateCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	const current = "v0.27.0"
	srv := signedUpdateServer(t, []byte("installed beta"), runtime.GOOS, runtime.GOARCH, "v0.26.0-beta.1", nil)
	defer srv.Close()
	bin := filepath.Join(t.TempDir(), "wanctl")
	flags := "-X main.buildVersion=" + current + " -X wanctl/internal/release.TrustedPublicKeys=" + wanrelease.TrustedPublicKeys
	if out, err := exec.Command("go", "build", "-ldflags", flags, "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	t.Setenv("WANCTL_RELEASE_BASE", srv.URL+"/dl")
	before, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"update", current}, {"update", "--help"}, {"help", "update"}} {
		out, code := runWanctl(t, bin, args...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, out)
		}
		want := "已在运行"
		if args[len(args)-1] != current {
			want = "vX.Y.Z-beta.N"
		}
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s: %s", want, out)
		}
	}
	for _, v := range []string{"dev", "v0.26.0-beta", "v0.26.0-beta.0", "v0.26.0-beta.01", "v0.26.0-rc.1", "v0.26.0-alpha.1", "v0.26.0-Beta.1", "v0.26.0+meta", "v0.26.0-beta.1+meta", "v0.26.0-beta.1.2"} {
		if out, code := runWanctl(t, bin, "update", v); code == 0 {
			t.Fatalf("accepted %s: %s", v, out)
		}
	}
	after, err := os.ReadFile(bin)
	if err != nil || !slices.Equal(before, after) {
		t.Fatal("same version / invalid request touched binary")
	}
	out, code := runWanctl(t, bin, "update", "--no-restart", "v0.26.0-beta.1")
	if code != 0 {
		t.Fatalf("%d: %s", code, out)
	}
	for _, want := range []string{"降级", "auto_update=on", "下次自动检查", "最新稳定版", "已安装 wanctl v0.26.0-beta.1"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s: %s", want, out)
		}
	}
	b, err := os.ReadFile(bin)
	if err != nil || string(b) != "installed beta" {
		t.Fatalf("binary = %q, %v", b, err)
	}
}

func TestSudoUpdatePassesVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sudo is Unix-only")
	}
	dir := t.TempDir()
	capture := filepath.Join(dir, "args")
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_TEST_SUDO_ARGS", capture)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$WANCTL_TEST_SUDO_ARGS\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := splitUpdateViaSudo(t.Context(), "/test/wanctl", "v0.26.0-beta.2"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(capture)
	if err != nil || string(got) != "/test/wanctl\nupdate\n--no-restart\nv0.26.0-beta.2\n" {
		t.Fatalf("%q, %v", got, err)
	}
}

func TestExplicitUpdateWithoutSources(t *testing.T) {
	t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
	t.Setenv("WANCTL_RELAY", "")
	t.Setenv("WANCTL_RELEASE_BASE", "")
	err := cmdUpdate(t.Context(), []string{"v0.26.0-beta.1"})
	if err == nil || !strings.Contains(err.Error(), "v0.26.0-beta.1") || !strings.Contains(err.Error(), "没有可尝试的发布来源") {
		t.Fatalf("%v", err)
	}
}
