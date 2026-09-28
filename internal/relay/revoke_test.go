package relay

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wanctl/internal/sessionauth"
)

// shareStore keeps shares in memory, behind the same admin calls the real
// store answers, so the revocation endpoints run as they do in production.
type shareStore struct {
	noopAdmin
	mu     sync.Mutex
	shares map[int]shareRow
}

type shareRow struct {
	owner, device, grantee string
	manage                 bool
}

func newShareStore(rows ...shareRow) *shareStore {
	s := &shareStore{shares: map[int]shareRow{}}
	for i, row := range rows {
		s.shares[i+1] = row
	}
	return s
}

func (s *shareStore) ACLGrant(caller, owner, device string) (Grant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, row := range s.shares {
		if row.owner == owner && row.device == device && row.grantee == caller {
			return Grant{Manage: row.manage}, true
		}
	}
	return Grant{}, false
}

func (s *shareStore) RevokeACL(namespace string, id int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if row, ok := s.shares[id]; ok && row.owner == namespace {
		delete(s.shares, id)
	}
	return nil
}

func (s *shareStore) RevokeACLMatch(namespace string, id int, device, grantee string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, row := range s.shares {
		if row.owner == namespace && (k == id || id == 0 && row.device == device && row.grantee == grantee) {
			delete(s.shares, k)
			return true, nil
		}
	}
	return false, nil
}

func (s *shareStore) FriendRemove(namespace, other string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, row := range s.shares {
		if row.owner == namespace && row.grantee == other || row.owner == other && row.grantee == namespace {
			delete(s.shares, k)
		}
	}
	return nil
}

func (s *shareStore) SetACLManage(namespace, device, grantee string, manage bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, row := range s.shares {
		if row.owner == namespace && row.device == device && row.grantee == grantee {
			row.manage = manage
			s.shares[k] = row
			return true, nil
		}
	}
	return false, nil
}

func (s *shareStore) RemoveDevice(namespace, device string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, row := range s.shares {
		if row.owner == namespace && row.device == device {
			delete(s.shares, k)
		}
	}
	return nil
}

const shareAdminSecret = "an-admin-secret-long-enough-to-be-accepted"

func sharingRelay(t *testing.T, store *shareStore) *Relay {
	t.Helper()
	r := New(EnvTokenStore("tok-alice:alice,tok-bob:bob"))
	r.SetACL(store)
	r.SetAdmin(store)
	r.SetAdminSecret(shareAdminSecret)
	t.Cleanup(func() {
		r.hmu.Lock()
		all := make(map[string]*httpSession, len(r.hsess))
		for sid, s := range r.hsess {
			all[sid] = s
		}
		r.hmu.Unlock()
		for sid, s := range all {
			r.closeHTTPSession(sid, s)
		}
	})
	return r
}

func post(r *Relay, path, token, body string) int {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else {
		req.Header.Set("X-Admin-Secret", shareAdminSecret)
	}
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	return rec.Code
}

func tunnelWrite(r *Relay, token, sid string) int {
	return post(r, "/h/up?session="+sid+"&role=client", token, "operation")
}

// Every way an owner takes a share back — or loses it with the friendship or
// the device — ends the sessions the grantee has open on that device at once,
// not just the grantee's next dial. The owner's own session carries on.
func TestRevokedShareClosesItsOpenSessions(t *testing.T) {
	revocations := map[string]func(r *Relay) int{
		"owner revokes the share": func(r *Relay) int {
			return post(r, "/u/shares/revoke", "tok-alice", `{"device":"owned","grantee":"bob"}`)
		},
		"admin revokes the share": func(r *Relay) int {
			return post(r, "/admin/acl/revoke", "", `{"namespace":"alice","id":1}`)
		},
		"owner removes the friend": func(r *Relay) int {
			return post(r, "/u/friends/remove", "tok-alice", `{"namespace":"bob"}`)
		},
		"grantee removes the friend": func(r *Relay) int {
			return post(r, "/u/friends/remove", "tok-bob", `{"namespace":"alice"}`)
		},
		"admin removes the friendship": func(r *Relay) int {
			return post(r, "/admin/friends/remove", "", `{"namespace":"alice","peer":"bob"}`)
		},
		"admin unbinds the device": func(r *Relay) int {
			return post(r, "/admin/devices/remove", "", `{"namespace":"alice","device":"owned"}`)
		},
	}
	for name, revoke := range revocations {
		t.Run(name, func(t *testing.T) {
			r := sharingRelay(t, newShareStore(shareRow{owner: "alice", device: "owned", grantee: "bob"}))
			registerHTTPAgent(t, r, "tok-alice", "alice/owned")
			code, shared := dialAs(t, r, "tok-bob", "alice/owned")
			if code != http.StatusOK {
				t.Fatalf("grantee's dial = %d (%s)", code, shared)
			}
			code, own := dialAs(t, r, "tok-alice", "alice/owned")
			if code != http.StatusOK {
				t.Fatalf("owner's dial = %d (%s)", code, own)
			}
			if code := tunnelWrite(r, "tok-bob", shared); code != http.StatusOK {
				t.Fatalf("grantee's write before the revocation = %d", code)
			}

			if code := revoke(r); code != http.StatusOK {
				t.Fatalf("revocation = %d", code)
			}

			if code := tunnelWrite(r, "tok-bob", shared); code == http.StatusOK {
				t.Fatal("the grantee's open session still took a write after the share was revoked")
			}
			if rec := downPoll(t, r, shared, "client", 0); rec.Code == http.StatusOK || rec.Code == http.StatusNoContent {
				t.Fatalf("the grantee's open session still answered a read with %d", rec.Code)
			}
			if code, _ := dialAs(t, r, "tok-bob", "alice/owned"); code != http.StatusForbidden {
				t.Fatalf("the grantee's new dial = %d, want 403", code)
			}
			if code := tunnelWrite(r, "tok-alice", own); code != http.StatusOK {
				t.Fatalf("the owner's own session = %d after revoking someone else's share", code)
			}
		})
	}
}

// Turning management off takes the device's control plane away, so a session
// opened with it ends; one opened without it, which the share still allows,
// carries on, and so does everything when management is turned on.
func TestManageOffClosesOnlySessionsHoldingTheControlPlane(t *testing.T) {
	for _, via := range []string{"owner", "admin"} {
		t.Run(via, func(t *testing.T) {
			store := newShareStore(shareRow{owner: "alice", device: "owned", grantee: "bob", manage: true})
			r := sharingRelay(t, store)
			registerHTTPAgent(t, r, "tok-alice", "alice/owned")
			setManage := func(on bool) {
				t.Helper()
				body := `{"device":"owned","grantee":"bob","manage":false}`
				if on {
					body = strings.Replace(body, "false", "true", 1)
				}
				token := "tok-alice"
				path := "/u/shares/manage"
				if via == "admin" {
					token, path = "", "/admin/acl/manage"
					body = strings.Replace(body, `{`, `{"namespace":"alice",`, 1)
				}
				if code := post(r, path, token, body); code != http.StatusOK {
					t.Fatalf("manage %v = %d", on, code)
				}
			}
			_, managing := dialAs(t, r, "tok-bob", "alice/owned")
			setManage(false)
			if code := tunnelWrite(r, "tok-bob", managing); code == http.StatusOK {
				t.Fatal("a session holding the control plane outlived management being turned off")
			}
			_, using := dialAs(t, r, "tok-bob", "alice/owned")
			setManage(false)
			setManage(true)
			if code := tunnelWrite(r, "tok-bob", using); code != http.StatusOK {
				t.Fatalf("a session the share still allows = %d", code)
			}
		})
	}
}

// Sessions on the WebSocket registry, and bridged ones, end the same way.
func TestRevokedShareClosesItsWebSocketSessions(t *testing.T) {
	r := sharingRelay(t, newShareStore(shareRow{owner: "alice", device: "owned", grantee: "bob"}))
	s := newPipeRelayServer(t, r.Handler())
	ctrl := registerWSAgent(t, r, s, "tok-alice", "owned")
	defer ctrl.Close()
	opens := json.NewDecoder(ctrl)

	// dialWS opens a session over the WebSocket registry and has the agent
	// pick it up, returning both legs.
	dialWS := func(token string) (controller, device net.Conn) {
		t.Helper()
		type dialed struct {
			nc  net.Conn
			err error
		}
		got := make(chan dialed, 1)
		go func() {
			nc, _, err := s.wsDial(context.Background(), "/dial?target=alice/owned", token)
			got <- dialed{nc, err}
		}()
		var open sessionauth.Open
		if err := opens.Decode(&open); err != nil {
			t.Fatalf("agent control: %v", err)
		}
		device, _, err := s.wsDial(context.Background(), open.URL, "tok-alice")
		if err != nil {
			t.Fatalf("agent session leg: %v", err)
		}
		d := <-got
		if d.err != nil {
			t.Fatalf("controller dial: %v", d.err)
		}
		return d.nc, device
	}
	roundTrip := func(controller, device net.Conn) error {
		if _, err := controller.Write([]byte("ping")); err != nil {
			return err
		}
		buf := make([]byte, 4)
		device.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, err := io.ReadFull(device, buf)
		device.SetReadDeadline(time.Time{})
		return err
	}
	sharedC, sharedD := dialWS("tok-bob")
	defer sharedC.Close()
	defer sharedD.Close()
	ownC, ownD := dialWS("tok-alice")
	defer ownC.Close()
	defer ownD.Close()
	// A bridged session: an HTTP controller on the WebSocket agent, which
	// picks the job up the way it does any other.
	picked := make(chan net.Conn, 1)
	go func() {
		var open sessionauth.Open
		if opens.Decode(&open) != nil {
			picked <- nil
			return
		}
		leg, _, _ := s.wsDial(context.Background(), open.URL, "tok-alice")
		picked <- leg
	}()
	code, bridged := dialAs(t, r, "tok-bob", "alice/owned")
	if code != http.StatusOK {
		t.Fatalf("bridged dial = %d (%s)", code, bridged)
	}
	bridgedD := <-picked
	if bridgedD == nil {
		t.Fatal("the agent did not pick up the bridged session")
	}
	defer bridgedD.Close()
	if err := roundTrip(sharedC, sharedD); err != nil {
		t.Fatalf("grantee's session before the revocation: %v", err)
	}

	if code := post(r, "/u/shares/revoke", "tok-alice", `{"device":"owned","grantee":"bob"}`); code != http.StatusOK {
		t.Fatalf("revocation = %d", code)
	}

	closed := make(chan error, 1)
	go func() { _, err := sharedC.Read(make([]byte, 1)); closed <- err }()
	select {
	case err := <-closed:
		if err == nil {
			t.Fatal("the grantee's WebSocket session delivered data after the share was revoked")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the grantee's WebSocket session stayed open after the share was revoked")
	}
	if code := tunnelWrite(r, "tok-bob", bridged); code == http.StatusOK {
		t.Fatal("the grantee's bridged session still took a write after the share was revoked")
	}
	if err := roundTrip(ownC, ownD); err != nil {
		t.Fatalf("the owner's own WebSocket session after revoking someone else's share: %v", err)
	}
	if _, resp, err := s.wsDial(context.Background(), "/dial?target=alice/owned", "tok-bob"); err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("the grantee's new WebSocket dial was not refused with 403: %v", err)
	}
}
