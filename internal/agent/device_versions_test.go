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
	"strings"
	"testing"
	"time"

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
					reports <- map[string]string{"wanctl_version": q.Get("wanctl_version"), "opencu_version": q.Get("opencu_version")}
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
			// Reconnection must rediscover the manifest; never cache the first report.
			for _, version := range []string{"0.3.99", "0.4.0", ""} {
				if version == "" {
					if err := os.Remove(manifest); err != nil {
						t.Fatal(err)
					}
				} else {
					writeManifest(version)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				done := make(chan error, 1)
				go func() { done <- a.Run(ctx) }()
				select {
				case got := <-reports:
					if got["wanctl_version"] != a.opts.Version || got["opencu_version"] != version {
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
