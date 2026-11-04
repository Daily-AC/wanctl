package opencu

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A default-source cache with a built-in engine. NewAndroidCache would load
// none on the host, and a non-empty base would count as AndroidURLEnv.
func builtInCache(t *testing.T, files, base, body, version string) *AndroidCache {
	t.Helper()
	c := NewAndroidCache(files, "")
	c.base = base
	if body != "" {
		c.builtIn = &builtInAndroidEngine{manifest: androidManifest(body, version), jar: body}
	}
	return c
}

type engineRelease struct {
	*httptest.Server
	checks, jars atomic.Int32
}

func serveEngine(t *testing.T, body, version string) *engineRelease {
	t.Helper()
	r := &engineRelease{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/opencu-android.json":
			r.checks.Add(1)
			json.NewEncoder(w).Encode(androidManifest(body, version))
		case "/opencu-android.jar":
			r.jars.Add(1)
			fmt.Fprint(w, body)
		}
	}))
	t.Cleanup(r.Close)
	return r
}

func acquireBytes(t *testing.T, c *AndroidCache) (AndroidEngine, string) {
	t.Helper()
	e, done, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done()
	r, err := e.Reader()
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return e, string(b)
}

func TestBuiltInEngineServesFirstUseWhenDownloadFails(t *testing.T) {
	var checks atomic.Int32
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		checks.Add(1)
		http.Error(w, "offline", http.StatusServiceUnavailable)
	}))
	defer down.Close()
	c := builtInCache(t, t.TempDir(), down.URL, "built in jar", "1.0.0")
	for range 2 {
		e, got := acquireBytes(t, c)
		if got != "built in jar" || e.Manifest.Version != "1.0.0" || e.Path != "" {
			t.Fatalf("engine = %q %+v", got, e)
		}
	}
	if checks.Load() != 1 {
		t.Fatalf("%d checks, want one within the interval", checks.Load())
	}
	if c.CachedVersion() != "1.0.0" {
		t.Fatalf("CachedVersion = %q", c.CachedVersion())
	}
}

func TestBuiltInEngineFreesAPhoneAlreadyThrottledAfterAFailedDownload(t *testing.T) {
	files := t.TempDir()
	first := NewAndroidCache(files, "")
	first.base = "http://127.0.0.1:1/"
	if _, _, err := first.Acquire(context.Background()); err == nil {
		t.Fatal("no engine and no network must fail without a built-in engine")
	}
	var checks atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { checks.Add(1) }))
	defer server.Close()
	c := builtInCache(t, files, server.URL, "built in jar", "1.0.0")
	if _, got := acquireBytes(t, c); got != "built in jar" {
		t.Fatalf("engine = %q", got)
	}
	if checks.Load() != 0 {
		t.Fatal("the 24-hour interval must still hold for the refresh")
	}
}

func TestNewerDownloadReplacesBuiltInEngine(t *testing.T) {
	r := serveEngine(t, "newer jar", "1.1.0")
	c := builtInCache(t, t.TempDir(), r.URL, "built in jar", "1.0.0")
	e, got := acquireBytes(t, c)
	if got != "newer jar" || e.Manifest.Version != "1.1.0" || e.Path == "" {
		t.Fatalf("engine = %q %+v", got, e)
	}
	if c.CachedVersion() != "1.1.0" {
		t.Fatalf("CachedVersion = %q", c.CachedVersion())
	}
	if _, got = acquireBytes(t, c); got != "newer jar" || r.jars.Load() != 1 {
		t.Fatalf("engine = %q after %d downloads", got, r.jars.Load())
	}
}

func TestBuiltInEngineBeatsAnOlderCache(t *testing.T) {
	files := t.TempDir()
	r := serveEngine(t, "old jar", "0.9.0")
	old := NewAndroidCache(files, "")
	old.base = r.URL
	if _, got := acquireBytes(t, old); got != "old jar" {
		t.Fatalf("seeded %q", got)
	}
	c := builtInCache(t, files, r.URL, "built in jar", "1.0.0")
	if c.CachedVersion() != "1.0.0" {
		t.Fatalf("CachedVersion = %q", c.CachedVersion())
	}
	e, got := acquireBytes(t, c)
	if got != "built in jar" || e.Path != "" {
		t.Fatalf("engine = %q %+v", got, e)
	}
}

func TestReleaseOfferingNothingNewerDownloadsNoJar(t *testing.T) {
	for _, version := range []string{"1.0.0", "0.9.0", "not.a.version"} {
		t.Run(version, func(t *testing.T) {
			r := serveEngine(t, "release jar", version)
			c := builtInCache(t, t.TempDir(), r.URL, "built in jar", "1.0.0")
			if _, got := acquireBytes(t, c); got != "built in jar" {
				t.Fatalf("engine = %q", got)
			}
			if r.checks.Load() != 1 || r.jars.Load() != 0 {
				t.Fatalf("checks %d, jars %d", r.checks.Load(), r.jars.Load())
			}
			if s := c.state(); s.Failure != "" || s.Checked.IsZero() {
				t.Fatalf("state = %+v", s)
			}
		})
	}
}

func TestBuiltInEngineDoesNotWaitOutAnUnreachableRelease(t *testing.T) {
	stop := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-stop }))
	defer server.Close()
	defer close(stop)
	c := builtInCache(t, t.TempDir(), server.URL, "built in jar", "1.0.0")
	c.budget = 100 * time.Millisecond
	start := time.Now()
	if _, got := acquireBytes(t, c); got != "built in jar" {
		t.Fatalf("engine = %q", got)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("took %v", took)
	}
}

func TestChosenSourceWinsOverBuiltInEngineUnlessItFails(t *testing.T) {
	r := serveEngine(t, "chosen jar", "0.1.0")
	c := NewAndroidCache(t.TempDir(), r.URL)
	c.builtIn = &builtInAndroidEngine{manifest: androidManifest("built in jar", "1.0.0"), jar: "built in jar"}
	if _, got := acquireBytes(t, c); got != "chosen jar" {
		t.Fatalf("engine = %q", got)
	}
	if c.CachedVersion() != "0.1.0" {
		t.Fatalf("CachedVersion = %q", c.CachedVersion())
	}

	down := NewAndroidCache(t.TempDir(), "http://127.0.0.1:1/")
	down.builtIn = c.builtIn
	if _, got := acquireBytes(t, down); got != "built in jar" {
		t.Fatalf("with the source down: engine = %q", got)
	}
}

// Replacing the jar without its manifest, or the reverse, must fail here rather
// than ship an APK that falls back to downloading.
func TestBuiltInAndroidEngineMatchesItsManifest(t *testing.T) {
	manifest, err := os.ReadFile("androidengine/opencu-android.json")
	if err != nil {
		t.Fatal(err)
	}
	jar, err := os.ReadFile("androidengine/opencu-android.jar")
	if err != nil {
		t.Fatal(err)
	}
	engine, err := parseBuiltInAndroidEngine(manifest, string(jar))
	if err != nil {
		t.Fatal(err)
	}
	if engine.manifest.Version == "" || int64(len(jar)) != engine.manifest.Size {
		t.Fatalf("manifest = %+v", engine.manifest)
	}
	if _, err = parseBuiltInAndroidEngine(manifest, string(jar[:len(jar)-1])); err == nil || !strings.Contains(err.Error(), "size") {
		t.Fatalf("short jar: %v", err)
	}
	flipped := append([]byte(nil), jar...)
	flipped[0] ^= 1
	if _, err = parseBuiltInAndroidEngine(manifest, string(flipped)); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("changed jar: %v", err)
	}
}

// A controller that goes away mid-refresh says nothing about the release, so
// the next call checks again instead of waiting out the interval.
func TestCancelledRefreshDoesNotUseUpTheInterval(t *testing.T) {
	var checks atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if checks.Add(1) == 1 {
			cancel()
			<-r.Context().Done()
			return
		}
		http.Error(w, "offline", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := builtInCache(t, t.TempDir(), srv.URL, "built in jar", "1.0.0")
	if _, release, err := c.Acquire(ctx); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
	if got := c.state(); !got.Checked.IsZero() || got.Failure != "" {
		t.Fatalf("a cancelled refresh was recorded: %+v", got)
	}
	acquireBytes(t, c)
	if checks.Load() != 2 {
		t.Fatalf("%d checks, want the next call to check again", checks.Load())
	}
	if c.state().Checked.IsZero() {
		t.Fatal("a real failure must still use up the interval")
	}
}
