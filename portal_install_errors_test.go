package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"wanctl/internal/agent"
	"wanctl/internal/release"
)

func TestPortalReleaseFailureSummary(t *testing.T) {
	const target = "v0.29.0-beta.9"
	const github = "https://github.com/Daily-AC/wanctl/releases/download/" + target
	const mirror = "https://mirror.example/dl"
	missing := func(base string) error {
		return &updateHTTPError{URL: base + "/manifest.json", Status: "404 Not Found", StatusCode: 404}
	}
	network := releaseTimeout(github + "/manifest.json")
	mismatch := &release.ExactVersionMismatchError{Offered: "v0.27.0", Requested: target}
	for _, tc := range []struct {
		name     string
		failures []error
		want     string
	}{
		{"all absent", []error{missing(github), missing(mirror)}, "version " + target + " was not found in the official releases"},
		{"mirror has another signed tag", []error{missing(github), mismatch}, "version " + target + " was not found in the official releases"},
		{"timeout then 404", []error{network, missing(mirror)}, "the device could not reach github.com"},
		{"404 then timeout", []error{missing(github), releaseTimeout(mirror + "/manifest.json")}, "the device could not reach mirror.example"},
		{"timeout then mismatch", []error{network, mismatch}, "the device could not reach github.com"},
		{"HTTP unavailable", []error{&updateHTTPError{URL: github + "/manifest.json", Status: "503 Service Unavailable", StatusCode: 503}, missing(mirror)}, "the device could not reach github.com"},
		{"signature failure", []error{errors.New("verify release manifest: invalid signature"), missing(mirror)}, "wanctl installation failed; check the device logs"},
		{"missing signature is not a missing version", []error{&updateHTTPError{URL: github + "/manifest.sig", Status: "404 Not Found", StatusCode: 404}, missing(mirror)}, "the device could not reach github.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := 0
			_, err := fetchPortalUpdate(target, []string{github, mirror}, func(string) (updateFetch, error) { e := tc.failures[n]; n++; return updateFetch{}, e })
			var failure *agent.InstallError
			if !errors.As(err, &failure) || failure.Reason != tc.want {
				t.Fatalf("%v: %#v", err, failure)
			}
			if strings.Contains(failure.Reason, "https:") || strings.Contains(failure.Reason, "\n") {
				t.Fatal(failure.Reason)
			}
			for _, e := range tc.failures {
				if !strings.Contains(err.Error(), e.Error()) {
					t.Fatalf("lost source diagnostic %v: %v", e, err)
				}
			}
		})
	}
}
func releaseTimeout(address string) error {
	return &url.Error{Op: "Get", URL: address, Err: context.DeadlineExceeded}
}

// Real HTTP responses and signed manifests exercise the production download
// functions; no executable is replaced by this test.
func TestPortalReleaseDownloadFailuresAndFallback(t *testing.T) {
	const target = "v0.29.0-beta.9"
	absent := httptest.NewServer(http.NotFoundHandler())
	defer absent.Close()
	signed := signedUpdateServer(t, []byte("verified bytes"), "linux", "amd64", "v0.27.0", nil)
	defer signed.Close()
	bases := []string{absent.URL, signed.URL + "/dl"}
	attempt := func(base string) (updateFetch, error) {
		path, v, err := downloadSignedVersion(t.Context(), base, t.TempDir(), "linux", "amd64", "v0.26.0", target)
		return updateFetch{path: path, version: v}, err
	}
	_, err := fetchPortalUpdate(target, bases, attempt)
	var failure *agent.InstallError
	if !errors.As(err, &failure) || failure.Reason != "version "+target+" was not found in the official releases" {
		t.Fatalf("%v / %+v", err, failure)
	}
	if !strings.Contains(err.Error(), "404 Not Found") || !strings.Contains(err.Error(), "签名清单版本") {
		t.Fatal(err)
	}
	right := signedUpdateServer(t, []byte("verified bytes"), "linux", "amd64", target, nil)
	defer right.Close()
	got, err := fetchPortalUpdate(target, []string{absent.URL, right.URL + "/dl"}, attempt)
	if err != nil || got.version != target {
		t.Fatalf("fallback: %+v %v", got, err)
	}
}
