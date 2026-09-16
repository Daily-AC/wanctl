package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wanctl/internal/delegation"
	"wanctl/internal/sessionauth"
)

type transportGrantStore struct {
	mu     sync.Mutex
	grants map[string]delegation.Access
}

func (s *transportGrantStore) Resolve(token string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.grants[token]
	return a.Namespace, ok && !a.Delegated
}
func (s *transportGrantStore) ResolveAccess(token string) (delegation.Access, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.grants[token]
	return a, ok
}
func (s *transportGrantStore) revoke(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.grants, token)
}
func testTransportGrant() delegation.Access {
	return delegation.Access{Namespace: "alice", CredentialID: "credential-one", Delegated: true, GrantID: "grant-one", ControllerFingerprint: "controller-fp", ExpiresAt: time.Now().Add(time.Minute), Devices: []delegation.Device{{Namespace: "alice", ID: "allowed", Fingerprint: "device-fp"}}}
}
func grantRequest(h http.Handler, method, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestDelegatedAdmissionRejectsEveryManagementAndAgentSurface(t *testing.T) {
	s := &transportGrantStore{grants: map[string]delegation.Access{"delegate": testTransportGrant()}}
	r := New(s)
	h := r.Handler()
	for _, route := range []struct{ method, path string }{
		{"GET", "/agent"}, {"GET", "/h/poll?device=allowed"}, {"POST", "/h/deregister?device=allowed"},
		{"GET", "/session/leaked"}, {"GET", "/agent/notify-policy?device=allowed&inst=x"}, {"POST", "/agent/events?device=allowed&inst=x"},
		{"GET", "/u/friends"}, {"POST", "/u/friends/request"}, {"POST", "/u/friends/accept"}, {"POST", "/u/friends/decline"}, {"POST", "/u/friends/remove"},
		{"GET", "/u/users/lookup"}, {"GET", "/u/shares"}, {"POST", "/u/shares/grant"}, {"POST", "/u/shares/manage"}, {"POST", "/u/shares/revoke"},
		{"GET", "/u/notify"}, {"POST", "/u/notify"}, {"POST", "/u/notify/test"}, {"GET", "/u/devices/notify"},
		{"POST", "/docs/groups"}, {"POST", "/docs/groups/delete"}, {"POST", "/docs/articles"}, {"POST", "/docs/articles/delete"},
		{"POST", "/admin/tokens/issue"}, {"POST", "/admin/enroll/mint"}, {"POST", "/admin/delegations/approve"},
	} {
		t.Run(route.path+route.method, func(t *testing.T) {
			rec := grantRequest(h, route.method, route.path, "delegate")
			if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestDelegatedScopeUsesCanonicalDeviceAndFiltersDiscovery(t *testing.T) {
	a := testTransportGrant()
	r := New(&transportGrantStore{grants: map[string]delegation.Access{"delegate": a}})
	r.agents["alice/allowed"] = &agentConn{ns: "alice", device: "allowed", name: "alias", delegation: true}
	r.agents["alice/hidden"] = &agentConn{ns: "alice", device: "hidden", name: "private-label"}
	key, auth, _, ok := r.dialAccessAllowed(a, "alias")
	if !ok || key != "alice/allowed" || auth.Capabilities != sessionauth.UseCapabilities || auth.GrantID != a.GrantID || auth.ControllerFingerprint != a.ControllerFingerprint {
		t.Fatalf("resolved scope: %s %+v %v", key, auth, ok)
	}
	for _, target := range []string{"hidden", "alice/hidden", "other/allowed"} {
		if _, _, _, ok := r.dialAccessAllowed(a, target); ok {
			t.Fatalf("allowed target %s", target)
		}
	}
	for _, route := range []string{"/peers", "/h/peers"} {
		rec := grantRequest(r.Handler(), "GET", route, "delegate")
		if rec.Code != 200 || strings.Contains(rec.Body.String(), "hidden") || strings.Contains(rec.Body.String(), "private-label") || !strings.Contains(rec.Body.String(), "allowed") {
			t.Fatalf("discovery leak: %d %s", rec.Code, rec.Body.String())
		}
	}
	// A device does not inherit access from a name after that name moves.
	r.agents["alice/allowed"].name = "renamed"
	r.agents["alice/hidden"].name = "alias"
	if _, _, _, ok := r.dialAccessAllowed(a, "alias"); ok {
		t.Fatal("scope moved with alias")
	}
}

func TestDelegatedHTTPSessionBindsCredentialAndClientRole(t *testing.T) {
	a, b := testTransportGrant(), testTransportGrant()
	b.CredentialID = "credential-two"
	b.GrantID = "grant-two"
	r := New(&transportGrantStore{grants: map[string]delegation.Access{"a": a, "b": b}})
	r.hsess["mine"] = &httpSession{callerNS: "alice", ownerNS: "alice", credentialID: a.CredentialID, toAgent: newSideQueue(), toClient: newSideQueue()}
	r.hsess["owner"] = &httpSession{callerNS: "alice", ownerNS: "alice", toAgent: newSideQueue(), toClient: newSideQueue()}
	h := r.Handler()
	for _, route := range []string{"/h/up", "/h/down", "/h/close"} {
		for _, tc := range []struct{ token, suffix string }{{"b", "?session=mine&role=client"}, {"a", "?session=mine&role=agent"}, {"a", "?session=owner&role=client"}} {
			method := "POST"
			if route == "/h/down" {
				method = "GET"
			}
			if rec := grantRequest(h, method, route+tc.suffix, tc.token); rec.Code != 404 {
				t.Fatalf("%s %s status=%d", route, tc.suffix, rec.Code)
			}
		}
	}
	if rec := grantRequest(h, "POST", "/h/up?session=mine&role=client", "a"); rec.Code != 200 {
		t.Fatalf("own upload: %d", rec.Code)
	}
	if rec := grantRequest(h, "POST", "/h/close?session=mine&role=client", "a"); rec.Code != 200 {
		t.Fatalf("own close: %d", rec.Code)
	}
}

func TestDelegatedDialRejectsOldAgent(t *testing.T) {
	a := testTransportGrant()
	r := New(&transportGrantStore{grants: map[string]delegation.Access{"a": a}})
	r.agents["alice/allowed"] = &agentConn{ns: "alice", device: "allowed"}
	for _, path := range []string{"/dial?target=allowed", "/h/dial?target=allowed"} {
		if rec := grantRequest(r.Handler(), "GET", path, "a"); rec.Code != 409 {
			t.Fatalf("old agent status=%d", rec.Code)
		}
	}
}

func TestDelegatedLeaseExpiresAndRevokes(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "expire", true: "revoke"}[revoke], func(t *testing.T) {
			a := testTransportGrant()
			if !revoke {
				a.ExpiresAt = time.Now().Add(100 * time.Millisecond)
			}
			s := &transportGrantStore{grants: map[string]delegation.Access{"a": a}}
			r := New(s)
			l := r.beginAccessLease("s", "alice/allowed", a, "a")
			defer l.close()
			closed := make(chan struct{})
			l.addCloser(func() { close(closed) })
			if revoke {
				s.revoke("a")
			}
			select {
			case <-closed:
			case <-time.After(3 * time.Second):
				t.Fatal("live lease not closed")
			}
			if l.valid() {
				t.Fatal("closed lease valid")
			}
			r.leaseMu.Lock()
			n := len(r.leases)
			r.leaseMu.Unlock()
			if n != 0 {
				t.Fatal("lease retained")
			}
		})
	}
}

func TestUpstreamPreservesDelegationAndDoesNotCacheIt(t *testing.T) {
	a := testTransportGrant()
	s := &transportGrantStore{grants: map[string]delegation.Access{"delegate": a}}
	r := New(s)
	r.SetAdminSecret("admin")
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	up := NewUpstreamTokenStore(srv.URL, "admin")
	chain := ChainTokenStore{EnvTokenStore("full:bob"), up}
	got, ok := ResolveAccess(chain, "delegate")
	want, _ := json.Marshal(a)
	actual, _ := json.Marshal(got)
	if !ok || string(actual) != string(want) {
		t.Fatalf("metadata lost: %+v %v", got, ok)
	}
	if _, ok := chain.Resolve("delegate"); ok {
		t.Fatal("legacy upstream widened delegation")
	}
	s.revoke("delegate")
	if _, ok := ResolveAccess(chain, "delegate"); ok {
		t.Fatal("cached revoked grant")
	}
}

func TestLegacyUpstreamCannotWidenWebFetchToken(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/admin/tokens/inspect" {
			http.NotFound(w, req)
			return
		}
		writeJSON(w, map[string]string{"namespace": "alice"})
	}))
	defer upstream.Close()
	up := NewUpstreamTokenStore(upstream.URL, "secret")
	if _, ok := up.Resolve("wfd_legacy-downgrade"); ok {
		t.Fatal("legacy resolution accepted delegated bearer")
	}
	if _, ok := ResolveAccess(up, "wfd_legacy-downgrade"); ok {
		t.Fatal("fallback erased delegation scope")
	}
	if _, ok := ResolveAccess(EnvTokenStore("wfd_static:alice"), "wfd_static"); ok {
		t.Fatal("static tokens accepted reserved delegation prefix")
	}
}

type slowDelegationTokens struct {
	release chan struct{}
	access  delegation.Access
}

func (s slowDelegationTokens) Resolve(string) (string, bool) { return "", false }
func (s slowDelegationTokens) ResolveAccess(string) (delegation.Access, bool) {
	<-s.release
	return s.access, true
}

func TestDelegatedExpiryDoesNotWaitForSlowRevalidation(t *testing.T) {
	a := testTransportGrant()
	a.ExpiresAt = time.Now().Add(1200 * time.Millisecond)
	store := slowDelegationTokens{release: make(chan struct{}), access: a}
	defer close(store.release)
	r := New(store)
	l := r.beginAccessLease("session", "alice/allowed", a, "token")
	defer l.close()
	select {
	case <-l.done:
	case <-time.After(2 * time.Second):
		t.Fatal("expiry blocked behind token store")
	}
}
