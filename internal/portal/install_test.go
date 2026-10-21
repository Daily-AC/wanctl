package portal

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wanctl/internal/client"
	"wanctl/internal/protocol"
)

func installPortal(t *testing.T, owner string, shared, manage, online bool) *Server {
	t.Helper()
	return newTestPortal(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/resolve-user":
			w.Write([]byte(`{"namespace":"alice","role":"admin"}`))
		case "/admin/devices":
			json.NewEncoder(w).Encode(map[string]any{"devices": []map[string]any{{"name": "box", "owner": owner, "shared": shared, "manage": manage, "online": online, "wanctl_version": "v0.26.0"}}})
		default:
			t.Errorf("unexpected outbound request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
}
func installRequest(method, body string, auth, csrf bool) *http.Request {
	r := httptest.NewRequest(method, "http://portal.test/api/devices/install?device=box", strings.NewReader(body))
	if auth {
		r.Header.Set("X-User", "alice@example.com")
	}
	if csrf {
		token := newCSRFToken()
		r.AddCookie(&http.Cookie{Name: csrfCookieName, Value: token})
		r.Header.Set("X-CSRF-Token", token)
		r.Header.Set("Origin", "http://portal.test")
	}
	return r
}
func TestInstallOwnerAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name, owner                        string
		shared, manage, auth, csrf, online bool
		want                               int
	}{
		{"owner", "alice", false, false, true, true, true, 200},
		{"share read", "bob", true, false, true, true, true, 403},
		{"share manage", "bob", true, true, true, true, true, 403},
		{"admin other's device", "bob", false, false, true, true, true, 403},
		{"anonymous", "alice", false, false, false, true, true, 401},
		{"csrf", "alice", false, false, true, false, true, 403},
		{"offline", "alice", false, false, true, true, false, 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := installPortal(t, tc.owner, tc.shared, tc.manage, tc.online)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, installRequest("POST", `{"package":"wanctl","version":"v0.27.0"}`, tc.auth, tc.csrf))
			if w.Code != tc.want {
				t.Fatalf("%d: %s", w.Code, w.Body)
			}
			// Owner passes the gate, reaches the (unwired) device and gets a stored failure.
			if tc.name == "owner" && !strings.Contains(w.Body.String(), "console not wired") {
				t.Fatal(w.Body)
			}
		})
	}
}
func TestInstallWireOldAgentAndProgress(t *testing.T) {
	for _, old := range []bool{true, false} {
		t.Run(map[bool]string{true: "old", false: "new"}[old], func(t *testing.T) {
			s := installPortal(t, "alice", false, false, true)
			local, remote := net.Pipe()
			defer remote.Close()
			dc := newDeviceConn(local)
			defer dc.close()
			s.dialer = &client.Client{}
			s.conns["alice/box"] = dc
			sent := make(chan struct{})
			go func() {
				defer close(sent)
				m, err := protocol.ReadMessage(remote)
				if err != nil {
					return
				}
				if m.Package != "wanctl" || m.Version != "v0.27.0" {
					t.Errorf("wrong request %+v", m)
				}
				if old {
					_ = protocol.WriteMessage(remote, protocol.Message{Kind: protocol.KindError, Data: json.RawMessage(`"unknown RPC kind"`)})
					return
				}
				_ = protocol.WriteMessage(remote, protocol.Message{Kind: protocol.KindPackageInstall})
				raw, _ := json.Marshal(protocol.InstallProgress{Package: "wanctl", Version: "v0.27.0", Stage: "done"})
				_ = protocol.WriteMessage(remote, protocol.Message{Kind: protocol.KindInstallProgress, Data: raw})
			}()
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, installRequest("POST", `{"package":"wanctl","version":"v0.27.0"}`, true, true))
			if w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			<-sent
			if old && !strings.Contains(w.Body.String(), oldInstallAgent) {
				t.Fatal(w.Body)
			}
			if !old {
				// A device's optimistic done is only installing until registration agrees.
				time.Sleep(10 * time.Millisecond)
				w = httptest.NewRecorder()
				s.Handler().ServeHTTP(w, installRequest("GET", "", true, false))
				if !strings.Contains(w.Body.String(), `"stage":"installing"`) {
					t.Fatal(w.Body)
				}
				w = httptest.NewRecorder()
				s.Handler().ServeHTTP(w, installRequest("POST", `{"package":"opencu","version":"latest"}`, true, true))
				if w.Code != 409 {
					t.Fatalf("concurrent %d %s", w.Code, w.Body)
				}
			}
		})
	}
}
func TestInstallCompletionAndTimeout(t *testing.T) {
	s := installPortal(t, "alice", false, false, true)
	for _, tc := range []struct {
		version string
		age     time.Duration
		want    string
	}{
		{"v0.26.0", time.Minute, "done"}, {"v0.27.0", time.Minute, "installing"}, {"v0.27.0", 11 * time.Minute, "failed"},
	} {
		s.installs.devices = map[string]*deviceInstall{"alice/box": {InstallProgress: protocol.InstallProgress{Package: "wanctl", Version: tc.version, Stage: "installing"}, Accepted: true, Started: time.Now().Add(-tc.age)}}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, installRequest("GET", "", true, false))
		if !strings.Contains(w.Body.String(), `"stage":"`+tc.want+`"`) {
			t.Fatal(w.Body)
		}
	}
}

func TestInstallProgressIsOwnerOnly(t *testing.T) {
	for _, tc := range []struct {
		owner                string
		shared, manage, auth bool
		want                 int
	}{
		{"alice", false, false, true, 200}, {"bob", true, false, true, 403}, {"bob", true, true, true, 403}, {"bob", false, false, true, 403}, {"alice", false, false, false, 401},
	} {
		s := installPortal(t, tc.owner, tc.shared, tc.manage, true)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, installRequest("GET", "", tc.auth, false))
		if w.Code != tc.want {
			t.Fatalf("%+v: %d %s", tc, w.Code, w.Body)
		}
	}
}
func TestInstallRejectsExtraInputs(t *testing.T) {
	for _, body := range []string{
		`{"package":"wanctl","version":"v0.27.0","url":"https://attacker.test/x"}`,
		`{"package":"wanctl","version":"v0.27.0"}{}`,
		`{"package":"wanctl","version":"../../evil"}`,
		`{"package":"shell","version":"latest"}`,
		`{"package":"opencu","version":"v0.4.0"}`,
	} {
		s := installPortal(t, "alice", false, false, true)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, installRequest("POST", body, true, true))
		if w.Code != 400 {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body)
		}
	}
}
