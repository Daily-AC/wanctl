package relay

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"wanctl/internal/delegation"
	"wanctl/internal/sessionauth"
)

type blockingDisableStore struct {
	noopAdmin
	disabled         atomic.Bool
	entered, release chan struct{}
}

func (s *blockingDisableStore) Resolve(token string) (string, bool) {
	switch token {
	case "owner-token":
		return "alice", true
	case "sharee-token":
		return "bob", !s.disabled.Load()
	}
	return "", false
}

func (s *blockingDisableStore) AccountDisabled(ns string) (*time.Time, bool, error) {
	if ns == "bob" && s.disabled.Load() {
		now := time.Now()
		return &now, true, nil
	}
	return nil, ns == "bob", nil
}

func (s *blockingDisableStore) SetAccountDisabled(ns string, disabled bool) (*time.Time, bool, error) {
	if ns == "bob" {
		s.disabled.Store(disabled)
	}
	return s.AccountDisabled(ns)
}

func (s *blockingDisableStore) ACLGrant(caller, owner, device string) (Grant, bool) {
	if caller != "bob" || owner != "alice" || device != "home-pc" {
		return Grant{}, false
	}
	// Pause the actual dial after credential resolution and before lease
	// registration, as a slow database ACL lookup can do in production.
	close(s.entered)
	<-s.release
	return Grant{Manage: false}, true
}

func TestDisableClosesDialAlreadyAuthorizing(t *testing.T) {
	store := &blockingDisableStore{entered: make(chan struct{}), release: make(chan struct{})}
	r := New(store)
	r.SetAdmin(store)
	r.SetACL(store)
	r.SetAdminSecret(disableTestSecret)
	t.Cleanup(func() { closeAllHTTPSessions(r) })
	registerHTTPAgent(t, r, "owner-token", "alice/home-pc")
	s := newPipeRelayServer(t, r.Handler())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type dialResult struct {
		conn net.Conn
		err  error
	}
	dialed := make(chan dialResult, 1)
	go func() {
		conn, _, err := s.wsDial(ctx, "/dial?target=alice/home-pc", "sharee-token")
		dialed <- dialResult{conn, err}
	}()
	select {
	case <-store.entered:
	case <-ctx.Done():
		t.Fatal("dial never reached its ACL lookup")
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/users/disable", strings.NewReader(`{"namespace":"bob","disabled":true}`))
	req.Header.Set("X-Admin-Secret", disableTestSecret)
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	close(store.release)
	if rr.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", rr.Code, rr.Body.String())
	}
	result := <-dialed
	if result.err != nil {
		return // Refusing the in-flight dial is a valid fix.
	}
	defer result.conn.Close()
	r.hmu.Lock()
	a := r.hagents["alice/home-pc"]
	r.hmu.Unlock()
	open := <-a.open
	// Alice's valid device can still send data through the hybrid session.
	req = httptest.NewRequest(http.MethodPost, "/h/up?session="+open.Session+"&role=agent", strings.NewReader("after-disable"))
	req.Header.Set("Authorization", "Bearer owner-token")
	rr = httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	result.conn.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, len("after-disable"))
	n, err := io.ReadFull(result.conn, b)
	if n == len(b) && err == nil && string(b) == "after-disable" {
		t.Fatalf("disabled sharee established a live WebSocket tunnel after disable returned: sessions=%s, device upload=%d, received=%q", rr.Header().Get("X-Session"), rr.Code, b)
	}
}

func TestDisabledNamespaceCannotPublishLease(t *testing.T) {
	for _, target := range []string{"alice/home-pc", "bob/home-pc"} {
		t.Run(target, func(t *testing.T) {
			store := &blockingDisableStore{}
			r := New(store)
			r.SetAdmin(store)
			store.disabled.Store(true)
			caller := "bob"
			if target == "bob/home-pc" {
				caller = "alice"
			}
			l := r.beginAccessLease("late", target, delegation.Access{Namespace: caller}, "token", sessionauth.UseCapabilities)
			defer l.close()
			select {
			case <-l.done:
			default:
				t.Fatal("disabled namespace retained a published lease")
			}
			r.leaseMu.Lock()
			n := len(r.leases)
			r.leaseMu.Unlock()
			if n != 0 {
				t.Fatal("closed lease remained registered")
			}
		})
	}
}
