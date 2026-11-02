package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wanctl/internal/desktop"
	"wanctl/internal/elevate"
	"wanctl/internal/opencu"
	"wanctl/internal/policy"
	"wanctl/internal/protocol"
)

func androidEnvironment(dir string) func(string) string {
	return func(k string) string {
		if k == "WANCTL_DEVICE_STATE_FILE" {
			return filepath.Join(dir, "state", "device.json")
		}
		return ""
	}
}
func androidDesktopExchange(t *testing.T, a *Agent, req protocol.DesktopRequest) protocol.DesktopResult {
	t.Helper()
	dev, ctl := net.Pipe()
	defer dev.Close()
	defer ctl.Close()
	ctl.SetDeadline(time.Now().Add(3 * time.Second))
	done := make(chan struct{})
	go func() {
		defer close(done)
		pending := a.doDesktopUsing(dev, "owner", "phone controller", protocol.Message{Action: "act", RequestID: desktop.NewID(), Desktop: &req}, sessionAudit{}, false, nil)
		if pending != nil {
			<-pending
		}
	}()
	res, _, err := desktop.ReadResult(ctl)
	if err != nil {
		t.Fatal(err)
	}
	ctl.Close()
	<-done
	return res
}
func TestAndroidAgentOpenCUGatesKeepLegacyWindowsOnly(t *testing.T) {
	a := newOptsAgent(t, Options{Mode: policy.ModeBypass})
	a.elevator = elevate.NewManager(false, "turn on 提权通道 in the wanctl app")
	a.configureAndroidOpenCU("android", androidEnvironment(t.TempDir()))
	for _, req := range []protocol.DesktopRequest{{Discover: true}, {Tool: "dynamic_android_tool", Arguments: json.RawMessage(`{}`)}} {
		res := androidDesktopExchange(t, a, req)
		if res.OpenCU == nil || res.OpenCU.Code != "OPENCU_START_FAILED" || !strings.Contains(res.Error, "提权通道") {
			t.Fatal("Android OpenCU still Windows-only", res)
		}
	}
	req := protocol.DesktopRequest{Actions: []protocol.DesktopAction{{Type: "key", Key: "enter"}}, ScreenshotID: desktop.NewID()}
	res := desktopExchange(t, a, protocol.Message{Action: "act", Desktop: &req}, false, nil)
	if res.Reason != protocol.DesktopWindowsOnly {
		t.Fatal("legacy gate changed", res)
	}
	a.configureAndroidOpenCU("android", func(string) string { return "" })
	if res := androidDesktopExchange(t, a, protocol.DesktopRequest{Discover: true}); !strings.Contains(res.Error, "needs the wanctl app") {
		t.Fatal("plain binary refusal", res)
	}
}
func TestAndroidAgentUsesExistingApprovalGateBeforeDownload(t *testing.T) {
	a := newOptsAgent(t, Options{Mode: policy.ModeNormal})
	a.setApprover(policy.DenyApprover{})
	a.elevator = elevate.NewManager(false, "disabled")
	a.configureAndroidOpenCU("android", androidEnvironment(t.TempDir()))
	res := androidDesktopExchange(t, a, protocol.DesktopRequest{Tool: "dynamic_phone_input", Arguments: json.RawMessage(`{"text":"PRIVATE"}`)})
	if res.Error != "command denied by device policy" {
		t.Fatal("approval skipped", res)
	}
}
func TestAndroidAgentCachedVersionInRegistrationWithoutNetwork(t *testing.T) {
	dir := t.TempDir()
	body := []byte("cached jar")
	h := sha256.Sum256(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".json") {
			json.NewEncoder(w).Encode(opencu.AndroidManifest{Name: "opencu-android", Version: "2.3.4", File: "opencu-android.jar", Size: int64(len(body)), SHA256: fmt.Sprintf("%x", h)})
		} else {
			w.Write(body)
		}
	}))
	cache := opencu.NewAndroidCache(dir, srv.URL)
	_, release, err := cache.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	srv.Close()
	a := newOptsAgent(t, Options{Version: "v0.28.0"})
	a.configureAndroidOpenCU("android", androidEnvironment(dir))
	if a.openCUVersion == nil {
		t.Fatal("Android version reader not configured")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.openCUVersion.start(ctx)
	deadline := time.Now().Add(time.Second)
	for a.versionFields()["opencu_version"] != "2.3.4" {
		if time.Now().After(deadline) {
			t.Fatal("registration lacks cached version", a.versionFields())
		}
		time.Sleep(time.Millisecond)
	}
	a.refreshOpenCUVersion("2.4.0")
	if a.versionFields()["opencu_version"] != "2.4.0" {
		t.Fatal("new engine version not published")
	}
}
