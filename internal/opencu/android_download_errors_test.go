package opencu

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// redirectingRelease answers the release request with a redirect to a signed
// asset URL and lets assetResponse decide what the asset host does.
func redirectingRelease(location string, asset func(*http.Request) (*http.Response, error)) http.RoundTripper {
	return roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "release.invalid" {
			return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{location}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}
		return asset(r)
	})
}

// A failed download is reported and remembered as its kind, host and HTTP
// status only, never with the user info, path or query of the URL it reached.
func TestAndroidDownloadErrorsKeepOnlyKindHostAndStatus(t *testing.T) {
	const location = "https://user:SYNTHETIC_PASSWORD@asset.invalid/private/engine?signature=SYNTHETIC_SECRET#fragment"
	for _, tc := range []struct {
		name, want string
		asset      func(*http.Request) (*http.Response, error)
	}{
		{"transport", "opencu-android.json download failed: request failed (asset.invalid)", func(*http.Request) (*http.Response, error) {
			return nil, errors.New("transport unavailable")
		}},
		{"status", "opencu-android.json download: HTTP 403 from asset.invalid", func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader("denied")), Request: r}, nil
		}},
		{"body", "opencu-android.json download failed: request failed (asset.invalid)", func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(io.MultiReader(strings.NewReader("{"), failingReader{})), Request: r}, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cache := NewAndroidCache(t.TempDir(), "https://release.invalid/")
			cache.client.Transport = redirectingRelease(location, tc.asset)
			_, _, err := cache.Acquire(context.Background())
			if err == nil {
				t.Fatal("expected a download error")
			}
			_, _, again := cache.Acquire(context.Background())
			persisted, _ := os.ReadFile(filepath.Join(cache.dir, "cache.json"))
			for _, text := range []string{err.Error(), again.Error(), string(persisted)} {
				for _, secret := range []string{"SYNTHETIC", "private", "signature", "fragment", "user:"} {
					if strings.Contains(text, secret) {
						t.Fatalf("download error keeps %q: %s", secret, text)
					}
				}
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(string(persisted), tc.want) {
				t.Fatalf("error %q / persisted %s, want %q", err, persisted, tc.want)
			}
		})
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("read https://asset.invalid/private/engine?signature=SYNTHETIC_SECRET: reset")
}
