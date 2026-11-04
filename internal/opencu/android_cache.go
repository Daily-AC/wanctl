package opencu

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"wanctl/internal/release"
	"wanctl/internal/versionmeta"
)

const AndroidURLEnv = "WANCTL_OPENCU_ANDROID_URL"
const AndroidReleaseURL = "https://github.com/Daily-AC/opencu/releases/latest/download/"
const MaxAndroidJar = 64 << 20
const AndroidRefresh = 24 * time.Hour

// androidRefreshBudget bounds a refresh when an engine is already available.
// A network that cannot reach the release must not stall the call that happens
// to be first in a 24-hour interval for the HTTP client's full 90 seconds.
const androidRefreshBudget = 10 * time.Second

// errBuiltInCurrent ends a refresh whose manifest offers nothing newer than the
// engine built into this binary, so there is no jar to download.
var errBuiltInCurrent = errors.New("the built-in Android engine is current")

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

// AndroidEngine is one acquired engine. It carries the bytes that were
// verified against the manifest, so placement never reopens the cache file:
// whatever happens to Path after Acquire cannot change what is uploaded.
type AndroidEngine struct {
	Manifest AndroidManifest
	Path     string // where the verified copy is cached, empty for the built-in engine; informational
	jar      string // the verified bytes, immutable
}

// Reader returns exactly the verified bytes, refusing an engine whose bytes do
// not match its manifest size or exceed the 64 MiB limit.
func (e AndroidEngine) Reader() (io.Reader, error) {
	if e.Manifest.Size <= 0 || e.Manifest.Size > MaxAndroidJar || int64(len(e.jar)) != e.Manifest.Size {
		return nil, errors.New("Android engine bytes do not match the verified manifest")
	}
	return io.LimitReader(strings.NewReader(e.jar), e.Manifest.Size), nil
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
	builtIn   *builtInAndroidEngine // nil outside Android builds
	override  bool                  // the source was chosen with AndroidURLEnv
	budget    time.Duration
}

// builtInAndroidEngine is the engine compiled into the signed binary. Its bytes
// are checked against its manifest when loaded, like a cached jar.
type builtInAndroidEngine struct {
	manifest AndroidManifest
	jar      string
}

func parseBuiltInAndroidEngine(manifest []byte, jar string) (*builtInAndroidEngine, error) {
	var m AndroidManifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		return nil, errors.New("invalid built-in Android engine manifest JSON")
	}
	if err := m.validate(); err != nil {
		return nil, err
	}
	if int64(len(jar)) != m.Size {
		return nil, errors.New("built-in Android engine size mismatch")
	}
	h := sha256.Sum256([]byte(jar))
	if hex.EncodeToString(h[:]) != m.SHA256 {
		return nil, errors.New("built-in Android engine SHA-256 mismatch")
	}
	return &builtInAndroidEngine{manifest: m, jar: jar}, nil
}

// builtInCovers reports whether the built-in engine should be used instead of
// m: a downloaded engine replaces it only by being newer. A source chosen with
// AndroidURLEnv is exempt, because whoever set it wants that engine; the
// built-in engine then only stands in when that source has supplied nothing.
func (c *AndroidCache) builtInCovers(m AndroidManifest) bool {
	if c.builtIn == nil || c.override {
		return false
	}
	cmp, err := release.CompareVersions("v"+m.Version, "v"+c.builtIn.manifest.Version)
	return err != nil || cmp <= 0
}

// NewAndroidCache keeps the cache inside the APK's config dir
// (files/wanctl, which the app passes as WANCTL_CONFIG_DIR). File operations
// refuse that whole tree, so a session that may write files can neither place
// engine bytes nor choose the cache metadata that selects them.
func NewAndroidCache(filesDir, base string) *AndroidCache {
	override := base != ""
	if base == "" {
		base = AndroidReleaseURL
	}
	return &AndroidCache{dir: filepath.Join(filesDir, "wanctl", "opencu"), base: base, client: &http.Client{Timeout: 90 * time.Second}, now: time.Now,
		builtIn: loadBuiltInAndroidEngine(), override: override, budget: androidRefreshBudget}
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
	cached := m.validate() == nil
	if cached {
		st, err := os.Lstat(c.path(m))
		cached = err == nil && st.Mode().IsRegular() && st.Size() == m.Size
	}
	if c.builtIn != nil && (!cached || c.builtInCovers(m)) {
		return c.builtIn.manifest.Version
	}
	if !cached {
		return ""
	}
	return m.Version
}
func (c *AndroidCache) Acquire(ctx context.Context) (AndroidEngine, func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.state()
	current := s.Current
	jar, cacheErr := c.load(current)
	good := cacheErr == nil
	if !good {
		if previous, err := c.load(s.Previous); err == nil {
			current, jar = s.Previous, previous
			s.Current, s.Previous = current, AndroidManifest{}
			_ = c.save(s)
			good = true
		}
	}
	now := c.now()
	if c.active == 0 && (s.Checked.IsZero() || now.Sub(s.Checked) >= AndroidRefresh) {
		lastChecked := s.Checked
		s.Checked = now
		// Record attempts as well as successes, including across agent restarts.
		err := os.MkdirAll(c.dir, 0700)
		if err == nil {
			err = c.save(s)
		}
		refresh := ctx
		if good || c.builtIn != nil {
			var cancel context.CancelFunc
			refresh, cancel = context.WithTimeout(ctx, c.budget)
			defer cancel()
		}
		var m AndroidManifest
		if err == nil {
			m, err = c.fetch(refresh)
		}
		covered := errors.Is(err, errBuiltInCurrent)
		if covered {
			err, s.Failure = nil, ""
			_ = c.save(s)
		}
		var fetched string
		if err == nil && !covered {
			fetched, err = c.load(m)
		}
		if err == nil && !covered {
			next := s
			next.Current, next.Failure = m, ""
			if good && current.SHA256 != m.SHA256 {
				next.Previous = current
			}
			err = c.save(next)
			if err == nil {
				s = next
				c.prune(s)
				current, jar = m, fetched
				good = true
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				// The controller went away; that says nothing about the release,
				// so it must not use up the interval.
				s.Checked = lastChecked
			} else {
				s.Failure = boundFailure(err.Error())
			}
			_ = c.save(s)
		}
	}
	path := c.path(current)
	if c.builtIn != nil && (!good || c.builtInCovers(current)) {
		current, jar, good, path = c.builtIn.manifest, c.builtIn.jar, true, ""
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
	return AndroidEngine{Manifest: current, Path: path, jar: jar}, release, nil
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
	_, err := c.load(m)
	return err
}

// load reads a cached jar into memory and returns it only if its size and
// SHA-256 match m. The returned bytes, not the file, are what gets placed.
func (c *AndroidCache) load(m AndroidManifest) (string, error) {
	if err := m.validate(); err != nil {
		return "", err
	}
	f, err := openRegular(c.path(m), m.Size)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var jar strings.Builder
	jar.Grow(int(m.Size))
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(&jar, h), io.LimitReader(f, m.Size+1))
	if err != nil {
		return "", err
	}
	if n != m.Size {
		return "", errors.New("cached jar size mismatch")
	}
	if hex.EncodeToString(h.Sum(nil)) != m.SHA256 {
		return "", errors.New("cached jar SHA-256 mismatch")
	}
	return jar.String(), nil
}

// maxAndroidFailure bounds the diagnostic kept with the cache state.
const maxAndroidFailure = 512

func boundFailure(s string) string {
	if len(s) <= maxAndroidFailure {
		return s
	}
	return strings.ToValidUTF8(s[:maxAndroidFailure], "")
}

// save writes only what state can read back. The refresh timestamp and the
// engine records must survive; the diagnostic is bounded, and dropped if the
// state would still exceed the read limit.
func (c *AndroidCache) save(s androidCacheState) error {
	s.Failure = boundFailure(s.Failure)
	b, err := json.Marshal(s)
	if err == nil && len(b) > maxVersionManifestBytes {
		s.Failure = ""
		b, err = json.Marshal(s)
	}
	if err != nil {
		return err
	}
	if len(b) > maxVersionManifestBytes {
		return errors.New("Android engine cache state exceeds 16 KiB")
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
		return nil, errors.New("invalid Android engine directory URL")
	}
	res, err := c.client.Do(req)
	if err != nil {
		failed := u
		var uerr *url.Error
		if errors.As(err, &uerr) {
			if p, perr := url.Parse(uerr.URL); perr == nil {
				failed = p
			}
		}
		return nil, downloadError(file, failed, 0, err)
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, downloadError(file, responseURL(res, u), res.StatusCode, nil)
	}
	return res, nil
}

// downloadError describes a failed release request by its kind, the host it
// went to and the HTTP status, and nothing else: the URL of a redirected
// download can carry credentials in its user info, path or query.
func downloadError(file string, u *url.URL, status int, err error) error {
	host := "unknown host"
	if u != nil && u.Host != "" && len(u.Host) <= 255 && strings.Trim(strings.ToLower(u.Host), "abcdefghijklmnopqrstuvwxyz0123456789.-:[]") == "" {
		host = strings.ToLower(u.Host)
	}
	if err == nil {
		return fmt.Errorf("%s download: HTTP %d from %s", file, status, host)
	}
	var dns *net.DNSError
	var cert *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var timeout net.Error
	var dial *net.OpError
	kind := "request failed"
	switch {
	case errors.Is(err, context.Canceled):
		kind = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		kind = "timed out"
	case errors.As(err, &dns):
		kind = "DNS lookup failed"
	case errors.As(err, &cert), errors.As(err, &record):
		kind = "TLS failed"
	case errors.As(err, &timeout) && timeout.Timeout():
		kind = "timed out"
	case errors.As(err, &dial):
		kind = "connection failed"
	}
	return fmt.Errorf("%s download failed: %s (%s)", file, kind, host)
}

func responseURL(res *http.Response, fallback *url.URL) *url.URL {
	if res.Request != nil && res.Request.URL != nil {
		return res.Request.URL
	}
	return fallback
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
		return m, downloadError("opencu-android.json", responseURL(res, nil), 0, err)
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
	if c.builtInCovers(m) {
		return m, errBuiltInCurrent
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
			return downloadError("opencu-android.jar", responseURL(res, nil), 0, err)
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

// openRegular opens path only if it is a regular file within limit. The open
// neither follows a final symlink nor blocks (a FIFO swapped in after the
// Lstat would otherwise hang it). What was opened must be the file the Lstat
// saw, so it is regular; its size is checked again because it may have grown.
func openRegular(path string, limit int64) (*os.File, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return nil, errors.New("cache file is not regular or exceeds its size limit")
	}
	if afterCacheLstat != nil {
		afterCacheLstat(path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|cacheOpenFlags, 0)
	if err != nil {
		return nil, err
	}
	current, err := f.Stat()
	// SameFile alone is not enough: Linux can hand a replacement the inode number
	// the removed file just freed, so check the opened file's type again.
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(st, current) || current.Size() > limit {
		f.Close()
		return nil, errors.New("cache file changed while opening")
	}
	return f, nil
}

// afterCacheLstat lets tests change the file between openRegular's Lstat and
// its open. It is always nil outside tests.
var afterCacheLstat func(path string)

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
