package portal

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode"
	"wanctl/internal/client"
	"wanctl/internal/protocol"
)

// Ported from the read-only adversarial review; exercises the real framed
// deviceConn and installation callback, not just a standalone sanitizer.
func reviewConnection(t *testing.T, s *Server, key string) (*deviceConn, net.Conn) {
	t.Helper()
	local, remote := net.Pipe()
	dc := newDeviceConn(local)
	t.Cleanup(func() { remote.Close(); dc.close() })
	s.dialer = &client.Client{}
	s.conns[key] = dc
	return dc, remote
}
func reviewStart(t *testing.T, s *Server, remote net.Conn, pkg, version string) {
	t.Helper()
	go func() {
		m, e := protocol.ReadMessage(remote)
		if e == nil {
			protocol.WriteMessage(remote, protocol.Message{Kind: m.Kind})
		}
	}()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, installRequest("POST", fmt.Sprintf(`{"package":%q,"version":%q}`, pkg, version), true, true))
	if w.Code != 200 {
		t.Fatalf("start %d %s", w.Code, w.Body)
	}
}
func reviewSendProgress(t *testing.T, dc *deviceConn, remote net.Conn, p protocol.InstallProgress) {
	t.Helper()
	raw, _ := json.Marshal(p)
	if e := protocol.WriteMessage(remote, protocol.Message{Kind: protocol.KindInstallProgress, Data: raw}); e != nil {
		t.Fatal(e)
	}
	// A subsequent response travels through the same reader after the callback.
	if e := protocol.WriteMessage(remote, protocol.Message{Kind: protocol.KindOK}); e != nil {
		t.Fatal(e)
	}
	select {
	case <-dc.respCh:
	case <-time.After(time.Second):
		t.Fatal("reader stalled")
	}
}
func TestInvariant5ProgressIsolation(t *testing.T) {
	s := installPortal(t, "alice", false, false, true)
	dc, remote := reviewConnection(t, s, "alice/box")
	other := &deviceInstall{InstallProgress: protocol.InstallProgress{Package: "opencu", Version: "latest", Stage: "downloading"}, Started: time.Now()}
	s.installs.devices = map[string]*deviceInstall{"bob/box": other, "alice/other": other}
	reviewStart(t, s, remote, "opencu", "latest")
	reviewSendProgress(t, dc, remote, protocol.InstallProgress{Package: "wanctl", Version: "v9.9.9", Stage: "done"})
	if s.installs.devices["alice/box"].Package != "opencu" || other.Stage != "downloading" {
		t.Fatal("cross package/device mutation")
	}
	reviewSendProgress(t, dc, remote, protocol.InstallProgress{Package: "opencu", Version: "0.4.1", Stage: "done"})
	if s.installs.devices["alice/box"].Version != "0.4.1" || other.Stage != "downloading" {
		t.Fatal("wrong progress ownership")
	}
}
func TestInvariant5ProgressBounds(t *testing.T) {
	s := installPortal(t, "alice", false, false, true)
	dc, remote := reviewConnection(t, s, "alice/box")
	reviewStart(t, s, remote, "opencu", "latest")
	payload := "<img src=x onerror=alert(1)>\x00\u202e" + strings.Repeat("x", 1<<20)
	reviewSendProgress(t, dc, remote, protocol.InstallProgress{Package: "opencu", Version: payload, Stage: "failed", Reason: payload})
	p := s.installs.devices["alice/box"]
	for _, r := range p.Reason {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			t.Fatal("retained control/format character")
		}
	}
	if len(p.Version) > 128 || len([]rune(p.Reason)) > 400 || strings.ContainsAny(p.Version, "<>\x00\u202e") {
		t.Fatalf("untrusted data retained: version=%d bytes reason=%d bytes (includes HTML/control chars)", len(p.Version), len(p.Reason))
	}
}

func TestProgressReasonCapsUnicodeCharacters(t *testing.T) {
	p := cleanInstallProgress(protocol.InstallProgress{Version: "0.4.1", Reason: "\x00\u202e" + strings.Repeat("界", 401)})
	if p.Version != "0.4.1" || len([]rune(p.Reason)) != 400 || p.Reason != strings.Repeat("界", 400) {
		t.Fatalf("version %q, reason runes %d", p.Version, len([]rune(p.Reason)))
	}
}
