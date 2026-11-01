package opencu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wanctl/internal/versionmeta"
)

const AndroidURLEnv = "WANCTL_OPENCU_ANDROID_URL"
const AndroidReleaseURL = "https://github.com/Daily-AC/opencu/releases/latest/download/"
const MaxAndroidJar = 64 << 20
const AndroidRefresh = 24 * time.Hour
const AndroidNeedsApp = "OpenCU on Android needs the wanctl app (APK); Termux and plain binaries are not supported"

type AndroidManifest struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	File    string `json:"file"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
}

func (m AndroidManifest) validate() error {
	b, err := hex.DecodeString(m.SHA256)
	if err != nil || len(b) != sha256.Size || strings.ToLower(m.SHA256) != m.SHA256 {
		return errors.New("invalid SHA-256 in Android engine manifest")
	}
	if m.Name != "opencu-android" || m.File != "opencu-android.jar" {
		return errors.New("invalid Android engine manifest name or file")
	}
	if m.Size <= 0 || m.Size > MaxAndroidJar {
		return errors.New("Android engine size must be between 1 byte and 64 MiB")
	}
	if m.Version == "" || versionmeta.Clean(m.Version) != m.Version {
		return errors.New("invalid Android engine version")
	}
	return nil
}

type AndroidEngine struct {
	Manifest AndroidManifest
	Path     string
}
type androidCacheState struct {
	Current  AndroidManifest `json:"current"`
	Previous AndroidManifest `json:"previous"`
	Checked  time.Time       `json:"checked"`
	Failure  string          `json:"failure,omitempty"`
}

// AndroidCache is shared by every controller on one APK agent. Acquire reserves
// a session before releasing the lock, so refresh cannot race a live engine.
type AndroidCache struct {
	mu        sync.Mutex
	dir, base string
	client    *http.Client
	now       func() time.Time
	active    int
}

func NewAndroidCache(filesDir, base string) *AndroidCache {
	if base == "" {
		base = AndroidReleaseURL
	}
	return &AndroidCache{dir: filepath.Join(filesDir, "opencu"), base: base, client: &http.Client{Timeout: 90 * time.Second}, now: time.Now}
}

// AndroidFilesDir uses the existing APK state-file contract, never the cwd or
// the shared /data/local/tmp fallback used by a standalone binary.
func AndroidFilesDir(goos string, getenv func(string) string) string {
	if goos != "android" {
		return ""
	}
	p := getenv("WANCTL_DEVICE_STATE_FILE")
	if !filepath.IsAbs(p) || filepath.Base(p) != "device.json" || filepath.Base(filepath.Dir(p)) != "state" {
		return ""
	}
	return filepath.Dir(filepath.Dir(p))
}
func (c *AndroidCache) path(m AndroidManifest) string {
	return filepath.Join(c.dir, "opencu-android-"+m.SHA256+".jar")
}
func (c *AndroidCache) state() androidCacheState {
	var s androidCacheState
	b, err := readRegular(filepath.Join(c.dir, "cache.json"), maxVersionManifestBytes)
	if err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

// CachedVersion performs metadata-only discovery; it never fetches or starts an engine.
func (c *AndroidCache) CachedVersion() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.state().Current
	if m.validate() != nil {
		return ""
	}
	st, err := os.Lstat(c.path(m))
	if err != nil || !st.Mode().IsRegular() || st.Size() != m.Size {
		return ""
	}
	return m.Version
}
func (c *AndroidCache) Acquire(ctx context.Context) (AndroidEngine, func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.state()
	current := s.Current
	cacheErr := c.verify(current)
	good := cacheErr == nil
	if !good && c.verify(s.Previous) == nil {
		current = s.Previous
		s.Current, s.Previous = current, AndroidManifest{}
		_ = c.save(s)
		good = true
	}
	now := c.now()
	if c.active == 0 && (s.Checked.IsZero() || now.Sub(s.Checked) >= AndroidRefresh) {
		s.Checked = now
		// Record attempts as well as successes, including across agent restarts.
		err := os.MkdirAll(c.dir, 0700)
		if err == nil {
			err = c.save(s)
		}
		var m AndroidManifest
		if err == nil {
			m, err = c.fetch(ctx)
		}
		if err == nil {
			next := s
			next.Current, next.Failure = m, ""
			if good && current.SHA256 != m.SHA256 {
				next.Previous = current
			}
			err = c.save(next)
			if err == nil {
				s = next
				c.prune(s)
				current = m
				good = true
			}
		}
		if err != nil {
			s.Failure = err.Error()
			_ = c.save(s)
		}
	}
	if !good {
		reason := s.Failure
		if reason == "" {
			reason = cacheErr.Error() + "; refresh is deferred until all sessions close or the 24-hour check interval expires"
		}
		return AndroidEngine{}, nil, fmt.Errorf("OPENCU_NOT_INSTALLED: Android engine: %s", reason)
	}
	c.active++
	var once sync.Once
	release := func() { once.Do(func() { c.mu.Lock(); c.active--; c.mu.Unlock() }) }
	return AndroidEngine{Manifest: current, Path: c.path(current)}, release, nil
}

// Only hash-named regular jar files belong to this cache. Keep the current
// and previous good versions; run only after publishing new metadata and with
// no sessions open, so neither a failed refresh nor a live engine loses a file.
func (c *AndroidCache) prune(s androidCacheState) {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		hash := strings.TrimSuffix(strings.TrimPrefix(name, "opencu-android-"), ".jar")
		if name != "opencu-android-"+hash+".jar" || len(hash) != 64 || strings.Trim(hash, "0123456789abcdef") != "" {
			continue
		}
		if hash == s.Current.SHA256 || hash == s.Previous.SHA256 || !entry.Type().IsRegular() {
			continue
		}
		_ = os.Remove(filepath.Join(c.dir, name))
	}
}

func (c *AndroidCache) verify(m AndroidManifest) error {
	if err := m.validate(); err != nil {
		return err
	}
	f, err := openRegular(c.path(m), m.Size)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, m.Size+1))
	if err != nil {
		return err
	}
	if n != m.Size {
		return errors.New("cached jar size mismatch")
	}
	if hex.EncodeToString(h.Sum(nil)) != m.SHA256 {
		return errors.New("cached jar SHA-256 mismatch")
	}
	return nil
}
func (c *AndroidCache) save(s androidCacheState) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return atomicCacheFile(c.dir, "cache.json", func(f *os.File) error { _, err := f.Write(b); return err })
}
func (c *AndroidCache) get(ctx context.Context, file string) (*http.Response, error) {
	u, err := url.Parse(c.base)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, errors.New("invalid Android engine directory URL")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + file
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	res, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s download failed: %w", file, err)
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("%s download: HTTP %d", file, res.StatusCode)
	}
	return res, nil
}
func (c *AndroidCache) fetch(ctx context.Context) (AndroidManifest, error) {
	var m AndroidManifest
	res, err := c.get(ctx, "opencu-android.json")
	if err != nil {
		return m, err
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxVersionManifestBytes+1))
	res.Body.Close()
	if err != nil {
		return m, fmt.Errorf("manifest download: %w", err)
	}
	if len(b) > maxVersionManifestBytes {
		return m, errors.New("Android engine manifest exceeds 16 KiB")
	}
	if err = json.Unmarshal(b, &m); err != nil {
		return m, errors.New("invalid Android engine manifest JSON")
	}
	if err = m.validate(); err != nil {
		return m, err
	}
	if c.verify(m) == nil {
		return m, nil
	}
	res, err = c.get(ctx, "opencu-android.jar")
	if err != nil {
		return m, err
	}
	defer res.Body.Close()
	err = atomicCacheFile(c.dir, filepath.Base(c.path(m)), func(f *os.File) error {
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(res.Body, m.Size+1))
		if err != nil {
			return fmt.Errorf("jar download: %w", err)
		}
		if n != m.Size {
			return fmt.Errorf("jar size mismatch: got %d, want %d", n, m.Size)
		}
		if hex.EncodeToString(h.Sum(nil)) != m.SHA256 {
			return errors.New("jar SHA-256 mismatch")
		}
		return nil
	})
	return m, err
}
func atomicCacheFile(dir, name string, write func(*os.File) error) error {
	f, err := os.CreateTemp(dir, ".opencu-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = write(f); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, name))
}
func openRegular(path string, limit int64) (*os.File, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return nil, errors.New("cache file is not regular or exceeds its size limit")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	current, err := f.Stat()
	if err != nil || !os.SameFile(st, current) {
		f.Close()
		return nil, errors.New("cache file changed while opening")
	}
	return f, nil
}
func readRegular(path string, limit int64) ([]byte, error) {
	f, err := openRegular(path, limit)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if len(b) > int(limit) {
		return nil, errors.New("cache file exceeds limit")
	}
	return b, err
}
