package opencu

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
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
	// Under the 16 KiB read limit only the diagnostic bound applies: the
	// beginning is kept, the rest is cut.
	medium := strings.Repeat("y", 4096)
	if err := c.save(androidCacheState{Current: current, Checked: checked, Failure: medium}); err != nil {
		t.Fatal(err)
	}
	if got := c.state().Failure; got != medium[:maxAndroidFailure] {
		t.Fatalf("persisted a %d-byte diagnostic, want its first %d bytes", len(got), maxAndroidFailure)
	}
}

// A refresh that fails with a long error that names no URL (a file-system
// error carrying the cache paths) is bounded where it is returned and where it
// is kept, still counts against the 24-hour interval across a restart, and
// leaves the last good engine usable.
func TestAndroidRefreshFailureWithALongLocalErrorIsBounded(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "last_good"}[warm], func(t *testing.T) {
			files := t.TempDir()
			for _, part := range []string{"a", "b", "c"} {
				files = filepath.Join(files, strings.Repeat(part, 100))
			}
			cache := NewAndroidCache(files, "https://release.invalid/")
			if err := os.MkdirAll(cache.dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if warm {
				good := androidManifest("genuine jar", "1.0.0")
				if err := os.WriteFile(cache.path(good), []byte("genuine jar"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := cache.save(androidCacheState{Current: good, Checked: time.Now().Add(-AndroidRefresh)}); err != nil {
					t.Fatal(err)
				}
			}
			next := androidManifest("new jar", "2.0.0")
			// The new jar cannot be stored: a directory holds its name.
			if err := os.Mkdir(cache.path(next), 0o700); err != nil {
				t.Fatal(err)
			}
			attempts := 0
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := "new jar"
				if strings.HasSuffix(r.URL.Path, ".json") {
					attempts++
					b, _ := json.Marshal(next)
					body = string(b)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			for i := range 2 {
				cache = NewAndroidCache(files, "https://release.invalid/")
				cache.client.Transport = transport
				_, release, err := cache.Acquire(context.Background())
				if release != nil {
					release()
				}
				if warm && err != nil {
					t.Fatalf("acquire %d lost the last good engine: %v", i, err)
				}
				if !warm && (err == nil || len(err.Error()) > maxAndroidFailure+64) {
					t.Fatalf("acquire %d returned an unbounded error of %d bytes", i, len(err.Error()))
				}
			}
			if attempts != 1 {
				t.Fatalf("%d refresh attempts within 24 hours", attempts)
			}
			kept := cache.state().Failure
			if kept == "" || len(kept) > maxAndroidFailure || !strings.Contains(kept, strings.Repeat("a", 100)) {
				t.Fatalf("kept failure of %d bytes: %.80q", len(kept), kept)
			}
		})
	}
}
