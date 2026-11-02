package opencu

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// A refresh that fails with a long diagnostic still counts against the
// 24-hour interval, across a restart, and leaves the last good engine usable.
func TestAndroidRefreshFailureKeepsThrottleAndLastGoodEngine(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "last_good"}[warm], func(t *testing.T) {
			dir := t.TempDir()
			cache := NewAndroidCache(dir, "https://release.invalid/")
			if warm {
				m := androidManifest("genuine jar", "1.0.0")
				if err := os.MkdirAll(cache.dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(cache.path(m), []byte("genuine jar"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := cache.save(androidCacheState{Current: m, Checked: time.Now().Add(-AndroidRefresh)}); err != nil {
					t.Fatal(err)
				}
			}
			attempts := 0
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host == "release.invalid" {
					attempts++
					return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://asset.invalid/" + strings.Repeat("x", 17000)}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
				}
				return nil, errors.New("transport unavailable")
			})
			for i := range 2 {
				cache = NewAndroidCache(dir, "https://release.invalid/")
				cache.client.Transport = transport
				_, release, err := cache.Acquire(context.Background())
				if release != nil {
					release()
				}
				if warm && err != nil {
					t.Fatalf("acquire %d lost the last good engine: %v", i, err)
				}
			}
			if attempts != 1 {
				t.Fatalf("%d refresh attempts within 24 hours", attempts)
			}
		})
	}
}

// Every state save writes is readable by state: an oversized diagnostic never
// costs the refresh timestamp or the current and previous engine records.
func TestAndroidCacheStateWithLongFailureStaysReadable(t *testing.T) {
	c := NewAndroidCache(t.TempDir(), "https://release.invalid/")
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	current, previous := androidManifest("two", "2.0.0"), androidManifest("one", "1.0.0")
	checked := time.Now().Add(-time.Hour)
	for _, failure := range []string{strings.Repeat("x", 20000), strings.Repeat("<", 20000), strings.Repeat("\xff", 20000), strings.Repeat("界", 7000)} {
		if err := c.save(androidCacheState{Current: current, Previous: previous, Checked: checked, Failure: failure}); err != nil {
			t.Fatal(err)
		}
		s := c.state()
		if s.Current != current || s.Previous != previous || !s.Checked.Equal(checked) {
			t.Fatalf("state lost its records: %+v", s)
		}
		if len(s.Failure) > maxAndroidFailure {
			t.Fatalf("persisted failure of %d bytes", len(s.Failure))
		}
	}
}
