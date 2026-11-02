package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wanctl/internal/opencu"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
)

type writeOnlyApprover struct{}

func (writeOnlyApprover) Ask(r policy.Request) policy.Decision {
	return policy.Decision{Allow: r.Kind == policy.KindWrite}
}

// A controller whose policy allows file writes but not act cannot install or
// select the Android engine that another controller's approved act starts.
// The APK's file layout is used: files/wanctl is the config dir, and the cache
// is located from a genuine download rather than assumed.
func TestAndroidEngineCacheRefusesFileWriteSessions(t *testing.T) {
	files := t.TempDir()
	t.Setenv("WANCTL_CONFIG_DIR", filepath.Join(files, "wanctl"))
	a, err := New(Options{RelayURL: "http://unused.invalid", Token: "synthetic", Mode: policy.ModeNormal, Shell: "/bin/sh"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	a.setApprover(writeOnlyApprover{})

	genuine := []byte("genuine release jar")
	genuineHash := fmt.Sprintf("%x", sha256.Sum256(genuine))
	release := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".json") {
			json.NewEncoder(w).Encode(opencu.AndroidManifest{Name: "opencu-android", Version: "1.0.0", File: "opencu-android.jar", Size: int64(len(genuine)), SHA256: genuineHash})
		} else {
			w.Write(genuine)
		}
	}))
	installed, done, err := opencu.NewAndroidCache(files, release.URL).Acquire(context.Background())
	release.Close()
	if err != nil {
		t.Fatal(err)
	}
	done()
	cacheDir := filepath.Dir(installed.Path)

	ctl := pipeController(t, a, 17)
	if ok, _ := a.gate(policy.Request{Kind: policy.KindExec, Cmd: "opencu discover", Peer: seededIdentity(t, 17).Fingerprint}); ok {
		t.Fatal("fixture unexpectedly allows act")
	}
	body := "SESSION_SELECTED_ENGINE_BYTES"
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
	manifest := opencu.AndroidManifest{Name: "opencu-android", Version: "6.6.6", File: "opencu-android.jar", Size: int64(len(body)), SHA256: hash}
	state, _ := json.Marshal(map[string]any{"current": manifest, "checked": time.Now()})
	for _, item := range []struct{ path, body string }{
		{filepath.Join(cacheDir, "opencu-android-"+hash+".jar"), body},
		{filepath.Join(cacheDir, "cache.json"), string(state)},
		{filepath.Join(cacheDir, "nested", "cache.json"), string(state)},
	} {
		if reply := roundTrip(t, ctl, protocol.Message{Kind: protocol.KindFileWrite, Path: item.path, Content: item.body}); reply.Kind != protocol.KindError {
			t.Errorf("file_write into the engine cache %q answered %q", item.path, reply.Kind)
		}
	}

	cache := opencu.NewAndroidCache(files, "https://unused.invalid/")
	engine, done, err := cache.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done()
	if engine.Manifest.SHA256 != genuineHash || cache.CachedVersion() != "1.0.0" {
		t.Fatalf("engine selected by a file-write session: version=%s hash=%s", engine.Manifest.Version, engine.Manifest.SHA256)
	}
}
