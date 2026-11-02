package opencu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func androidManifest(body, version string) AndroidManifest {
	h := sha256.Sum256([]byte(body))
	return AndroidManifest{"opencu-android", version, "opencu-android.jar", int64(len(body)), hex.EncodeToString(h[:])}
}
func TestAndroidCacheRefreshReservationsPersistenceAndPreviousGood(t *testing.T) {
	body := "jar version one"
	manifest := androidManifest(body, "1.0.0")
	checks, jars := 0, 0
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/opencu-android.json":
			checks++
			if fail {
				http.Error(w, "offline", 503)
				return
			}
			json.NewEncoder(w).Encode(manifest)
		case "/opencu-android.jar":
			jars++
			fmt.Fprint(w, body)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	cache := NewAndroidCache(dir, server.URL+"/")
	now := time.Now()
	cache.now = func() time.Time { return now }
	if cache.CachedVersion() != "" || checks != 0 {
		t.Fatal("version fetched")
	}
	first, release, err := cache.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if checks != 1 || jars != 1 || cache.CachedVersion() != "1.0.0" {
		t.Fatal("first fetch/version")
	}
	st, err := os.Stat(first.Path)
	if err != nil || st.Mode().Perm() != 0600 || !strings.HasSuffix(first.Path, manifest.SHA256+".jar") {
		t.Fatalf("path/mode %s %v", first.Path, err)
	}
	now = now.Add(48 * time.Hour)
	body = "jar version two"
	manifest = androidManifest(body, "2.0.0")
	second, release2, err := cache.Acquire(context.Background())
	if err != nil || second != first || checks != 1 {
		t.Fatal("refreshed with session open", err)
	}
	release()
	release() // once-only reservation release
	third, release3, err := cache.Acquire(context.Background())
	if err != nil || third != first || checks != 1 {
		t.Fatal("another session lost reservation")
	}
	release2()
	release3()
	second, release2, err = cache.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release2()
	if second.Manifest.Version != "2.0.0" || checks != 2 || jars != 2 {
		t.Fatal("refresh missing")
	}
	if b, err := os.ReadFile(first.Path); err != nil || string(b) != "jar version one" {
		t.Fatal("previous good removed")
	}
	// A new agent uses the persisted attempt time, not a fresh daily budget.
	cache = NewAndroidCache(dir, server.URL)
	cache.now = func() time.Time { return now }
	_, r, err := cache.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r()
	if checks != 2 {
		t.Fatal("restart repeated check")
	}
	now = now.Add(AndroidRefresh)
	fail = true
	got, r, err := cache.Acquire(context.Background())
	if err != nil || got != second {
		t.Fatal("failed refresh lost cache", err)
	}
	r()
	_, r, err = cache.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r()
	if checks != 3 {
		t.Fatal("failed refresh retried before 24 h")
	}
	// The previous good copy also rescues local corruption, without network.
	if err = os.WriteFile(second.Path, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	got, r, err = cache.Acquire(context.Background())
	if err != nil || got != first {
		t.Fatalf("previous fallback %+v %v", got, err)
	}
	r()
}
func TestAndroidCacheRejectsUntrustedDownloads(t *testing.T) {
	for _, kind := range []string{"manifest size", "manifest JSON", "name", "file traversal", "hash syntax", "hash mismatch", "jar too short", "jar too long", "size limit", "zero size", "HTTP 404"} {
		t.Run(kind, func(t *testing.T) {
			body := "verified jar"
			m := androidManifest(body, "1.2.3")
			manifestBytes := []byte(nil)
			want := ""
			switch kind {
			case "manifest size":
				manifestBytes = []byte(strings.Repeat("x", maxVersionManifestBytes+1))
				want = "16 KiB"
			case "manifest JSON":
				manifestBytes = []byte("{")
				want = "JSON"
			case "name":
				m.Name = "other"
				want = "name or file"
			case "file traversal":
				m.File = "../../evil.jar"
				want = "name or file"
			case "hash syntax":
				m.SHA256 = "$(id)"
				want = "SHA-256"
			case "hash mismatch":
				body = "wrong hash!!"
				want = "SHA-256 mismatch"
			case "jar too short":
				body = "short"
				want = "size mismatch"
			case "jar too long":
				body += "extra"
				want = "size mismatch"
			case "size limit":
				m.Size = MaxAndroidJar + 1
				want = "64 MiB"
			case "zero size":
				m.Size = 0
				want = "size"
			case "HTTP 404":
				want = "HTTP 404"
			}
			if manifestBytes == nil {
				manifestBytes, _ = json.Marshal(m)
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if kind == "HTTP 404" {
					http.NotFound(w, r)
					return
				}
				if r.URL.Path == "/opencu-android.json" {
					w.Write(manifestBytes)
				} else if r.URL.Path == "/opencu-android.jar" {
					fmt.Fprint(w, body)
				} else {
					t.Error("manifest controlled URL", r.URL.Path)
				}
			}))
			defer server.Close()
			cache := NewAndroidCache(t.TempDir(), server.URL)
			_, _, err := cache.Acquire(context.Background())
			if err == nil || !strings.Contains(err.Error(), "OPENCU_NOT_INSTALLED") || !strings.Contains(err.Error(), want) {
				t.Fatalf("error %v, want %s", err, want)
			}
			before := calls
			_, _, _ = cache.Acquire(context.Background())
			if calls != before {
				t.Fatal("repeated failed check")
			}
			files, _ := filepath.Glob(filepath.Join(cache.dir, "*.jar"))
			temps, _ := filepath.Glob(filepath.Join(cache.dir, ".opencu-*"))
			if len(files)+len(temps) != 0 {
				t.Fatalf("unverified/temporary files left: %v %v", files, temps)
			}
		})
	}
}
func TestAndroidCacheKeepsGoodEngineAfterBadHash(t *testing.T) {
	body := "good"
	m := androidManifest(body, "1.0.0")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".json") {
			json.NewEncoder(w).Encode(m)
		} else {
			fmt.Fprint(w, body)
		}
	}))
	defer server.Close()
	c := NewAndroidCache(t.TempDir(), server.URL)
	now := time.Now()
	c.now = func() time.Time { return now }
	old, r, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r()
	now = now.Add(AndroidRefresh)
	m = androidManifest("new!", "2.0.0")
	body = "bad!"
	current, r, err := c.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r()
	if current != old || c.CachedVersion() != "1.0.0" {
		t.Fatal("bad refresh replaced good engine")
	}
}
func TestAndroidFilesDirectoryAndMetadataOnlyVersion(t *testing.T) {
	files := t.TempDir()
	env := map[string]string{"WANCTL_DEVICE_STATE_FILE": filepath.Join(files, "state", "device.json")}
	get := func(k string) string { return env[k] }
	if AndroidFilesDir("android", get) != files || AndroidFilesDir("linux", get) != "" {
		t.Fatal("platform/file dir")
	}
	for _, path := range []string{"", "relative/state/device.json", "/tmp/device.json"} {
		env["WANCTL_DEVICE_STATE_FILE"] = path
		if AndroidFilesDir("android", get) != "" {
			t.Fatal("accepted standalone environment")
		}
	}
	c := NewAndroidCache(files, "http://unused.invalid")
	os.MkdirAll(c.dir, 0700)
	m := androidManifest("body", "9.8.7")
	os.WriteFile(c.path(m), []byte("body"), 0600)
	c.save(androidCacheState{Current: m})
	if c.CachedVersion() != "9.8.7" {
		t.Fatal("cached version missing")
	}
	os.Remove(c.path(m))
	if c.CachedVersion() != "" {
		t.Fatal("missing engine reported installed")
	}
	os.Mkdir(c.path(m), 0700)
	if c.CachedVersion() != "" {
		t.Fatal("directory reported engine")
	}
}

func TestAndroidCacheRetainsPreviousAcrossUnchangedManifestAndPrunesOlder(t *testing.T) {
	body := "one"
	manifest := androidManifest(body, "1.0.0")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".json") {
			json.NewEncoder(w).Encode(manifest)
		} else {
			fmt.Fprint(w, body)
		}
	}))
	defer server.Close()
	c := NewAndroidCache(t.TempDir(), server.URL)
	now := time.Now()
	c.now = func() time.Time { return now }
	fetch := func() AndroidEngine {
		t.Helper()
		e, r, err := c.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		r()
		now = now.Add(AndroidRefresh)
		return e
	}
	first := fetch()
	body = "two"
	manifest = androidManifest(body, "2.0.0")
	second := fetch()
	fetch()
	if c.state().Previous != first.Manifest {
		t.Fatal("unchanged manifest forgot previous good copy")
	}
	body = "three"
	manifest = androidManifest(body, "3.0.0")
	fetch()
	if _, err := os.Stat(first.Path); !os.IsNotExist(err) {
		t.Fatal("oldest jar not pruned", err)
	}
	if _, err := os.Stat(second.Path); err != nil {
		t.Fatal("previous jar pruned", err)
	}
}
