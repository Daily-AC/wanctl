package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"wanctl/internal/versionmeta"

	"github.com/coder/websocket"
	"wanctl/internal/limits"
	"wanctl/internal/wsconn"
)

// The registration decoder from relay 2e4ecf7, deliberately frozen without the
// new fields. It uses a limited stream and does NOT DisallowUnknownFields.
// The live new agent below must work with this pre-feature decoder.
func decodeOldRegistration(reader io.Reader) error {
	body := &io.LimitedReader{R: reader, N: limits.RelayControlBodyBytes}
	dec := json.NewDecoder(body)
	var reg struct {
		Op, Device, Fingerprint, Inst, Name string
		DeviceID                            string `json:"device_id"`
		Delegation                          string `json:"delegation"`
	}
	if err := dec.Decode(&reg); err != nil {
		return err
	}
	if reg.Op != "register" || reg.Device == "" {
		return errors.New("expected register")
	}
	return nil
}

func TestVersionReportsAndOldRelayCompatibility(t *testing.T) {
	for _, tr := range []string{"ws", "http"} {
		t.Run(tr, func(t *testing.T) {
			t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
			dir := t.TempDir()
			t.Setenv("LOCALAPPDATA", dir)
			if err := os.Mkdir(filepath.Join(dir, "OpenCU"), 0700); err != nil {
				t.Fatal(err)
			}
			manifest := filepath.Join(dir, "OpenCU", "opencu.json")
			writeManifest := func(v string) {
				raw, _ := json.Marshal(map[string]any{"version": v, "mcp": map[string]any{"command": filepath.Join(dir, "must-not-start")}})
				if err := os.WriteFile(manifest, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			reports := make(chan map[string]string, 4)
			errs := make(chan error, 4)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/agent":
					c, err := websocket.Accept(w, r, nil)
					if err != nil {
						errs <- err
						return
					}
					conn := wsconn.FromAccepted(r.Context(), c)
					defer conn.Close()
					// Capture the exact wire bytes, then feed them to the old decoder.
					var raw json.RawMessage
					if err = json.NewDecoder(conn).Decode(&raw); err != nil {
						errs <- err
						return
					}
					if err = decodeOldRegistration(strings.NewReader(string(raw))); err != nil {
						errs <- err
						return
					}
					var reg map[string]string
					if err = json.Unmarshal(raw, &reg); err != nil {
						errs <- err
						return
					}
					reports <- reg
					io.Copy(io.Discard, conn)
				case "/h/poll":
					// 2e4ecf7 handleHPoll only reads known query keys (no whitelist).
					q := r.URL.Query()
					if q.Get("device") == "" || q.Get("device_id") != q.Get("device") || q.Get("inst") == "" {
						http.Error(w, "bad old registration", 400)
						return
					}
					report := map[string]string{"wanctl_version": q.Get("wanctl_version")}
					if q.Has("opencu_version") {
						report["opencu_version"] = q.Get("opencu_version")
					}
					reports <- report
					<-r.Context().Done()
				default:
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			defer srv.Close()
			a, err := New(Options{RelayURL: "ws" + strings.TrimPrefix(srv.URL, "http"), Token: "tok", Name: "versions", Transport: tr, Version: "v1.2.3-beta.4"})
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			// Simulate publication from the Windows-only background reader. A cache
			// containing even worse data than the capped reader allows must remain
			// safe on the wire to a pre-version relay.
			a.openCUVersion = &installedVersionCache{}
			a.openCUVersion.once.Do(func() {})
			for _, version := range []string{"0.3.99", strings.Repeat("a", 70<<10), "bad\nvalue", ""} {
				if version == "" {
					if err := os.Remove(manifest); err != nil {
						t.Fatal(err)
					}
				} else {
					writeManifest(version)
				}
				if version != "" {
					raw, err := os.ReadFile(manifest)
					if err != nil {
						t.Fatal(err)
					}
					var m struct{ Version string }
					if err := json.Unmarshal(raw, &m); err != nil {
						t.Fatal(err)
					}
					a.openCUVersion.mu.Lock()
					a.openCUVersion.value = m.Version
					a.openCUVersion.mu.Unlock()
				} else {
					a.openCUVersion.mu.Lock()
					a.openCUVersion.value = ""
					a.openCUVersion.mu.Unlock()
				}
				a.opts.Version = version // includes the oversized and invalid build-string cases
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				done := make(chan error, 1)
				go func() { done <- a.Run(ctx) }()
				select {
				case got := <-reports:
					if got["wanctl_version"] != versionmeta.Clean(a.opts.Version) || got["opencu_version"] != versionmeta.Clean(version) {
						t.Errorf("report=%v", got)
					}
				case err := <-errs:
					t.Error(err)
				case <-ctx.Done():
					t.Error("agent did not register")
				}
				cancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("agent did not stop")
				}
			}
		})
	}
}

func TestSlowDiscoveryDoesNotDelayEitherRegistrationOrClose(t *testing.T) {
	for _, tr := range []string{"ws", "http"} {
		t.Run(tr, func(t *testing.T) {
			t.Setenv("WANCTL_CONFIG_DIR", t.TempDir())
			entered, release := make(chan struct{}), make(chan struct{})
			defer close(release)
			var reads atomic.Int32
			received := make(chan map[string]string, 8)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/agent" {
					c, err := websocket.Accept(w, r, nil)
					if err != nil {
						return
					}
					conn := wsconn.FromAccepted(r.Context(), c)
					defer conn.Close()
					var reg map[string]string
					if json.NewDecoder(conn).Decode(&reg) == nil {
						received <- reg
					}
					io.Copy(io.Discard, conn)
				} else if r.URL.Path == "/h/poll" {
					q := r.URL.Query()
					reg := map[string]string{"wanctl_version": q.Get("wanctl_version")}
					if q.Has("opencu_version") {
						reg["opencu_version"] = q.Get("opencu_version")
					}
					received <- reg
					<-r.Context().Done()
				} else {
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			defer srv.Close()
			a, err := New(Options{RelayURL: "ws" + strings.TrimPrefix(srv.URL, "http"), Token: "tok", Name: "versions", Transport: tr, Version: "v1.0.0"})
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && a.openCUVersion != nil {
				t.Fatal("non-Windows reader created")
			}
			// Inject a blocked Windows profile/file read without requiring Windows here.
			a.openCUVersion = &installedVersionCache{read: func() string {
				if reads.Add(1) == 1 {
					close(entered)
				}
				<-release
				return "0.3.99"
			}}
			for i := 0; i < 3; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { done <- a.Run(ctx) }()
				select {
				case got := <-received:
					if got["wanctl_version"] != "v1.0.0" || got["opencu_version"] != "" {
						t.Fatal(got)
					}
				case <-time.After(time.Second):
					t.Fatal("discovery delayed registration")
				}
				cancel()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("discovery delayed cancellation")
				}
			}
			if runtime.GOOS != "windows" {
				a.openCUVersion = newInstalledVersionCache()
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan error, 1)
				go func() { done <- a.Run(ctx) }()
				select {
				case got := <-received:
					if _, ok := got["opencu_version"]; ok {
						t.Fatal("non-Windows transport sent OpenCU")
					}
				case <-time.After(time.Second):
					t.Fatal("non-Windows registration stalled")
				}
				cancel()
				<-done
			}
			<-entered
			if reads.Load() != 1 {
				t.Fatal("spawned replacement readers")
			}
			closed := make(chan struct{})
			go func() { a.Close(); close(closed) }()
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("Close joined blocked metadata I/O")
			}
		})
	}
}

func TestVersionCacheRefreshIsRateLimitedAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var reads atomic.Int32
	cache := &installedVersionCache{read: func() string { reads.Add(1); return "0.3.99" }}
	cache.start(ctx)
	until := time.Now().Add(time.Second)
	for {
		cache.mu.RLock()
		known := cache.value == "0.3.99"
		cache.mu.RUnlock()
		if known {
			break
		}
		if time.Now().After(until) {
			t.Fatal("cache never populated")
		}
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < 100; i++ {
		cache.start(ctx)
	}
	cancel()
	time.Sleep(20 * time.Millisecond)
	if reads.Load() != 1 || versionRefreshInterval < time.Minute {
		t.Fatal("reader not rate limited")
	}
	if runtime.GOOS != "windows" {
		t.Setenv("LOCALAPPDATA", t.TempDir())
		a := &Agent{opts: Options{Version: "v1.0.0"}, openCUVersion: newInstalledVersionCache()}
		if _, ok := a.versionFields()["opencu_version"]; ok {
			t.Fatal("non-Windows field must be absent")
		}
	}
}
