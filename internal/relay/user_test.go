package relay

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type userEndpointStore struct {
	noopAdmin
	friends       []Friend
	users         map[string]bool
	requestErr    error
	decisionErr   error
	grantErr      error
	grants        map[int]string
	revoked       map[int]bool
	requestActor  string
	requestPeer   string
	requestPortal string
	grantActor    string
	grantDevice   string
	grantGrantee  string
	given         []map[string]any
	received      []ReceivedShare
}

func (s *userEndpointStore) ListFriends(string) ([]Friend, error) {
	return s.friends, nil
}

func (s *userEndpointStore) LookupUser(namespace string) (bool, error) {
	return s.users[namespace], nil
}

func (s *userEndpointStore) ListACL(string) ([]map[string]any, error) {
	return s.given, nil
}

func (s *userEndpointStore) ListReceivedACL(string) ([]ReceivedShare, error) {
	return s.received, nil
}

func (s *userEndpointStore) FriendRequest(actor, peer, portal string) (string, error) {
	s.requestActor, s.requestPeer, s.requestPortal = actor, peer, portal
	return "pending", s.requestErr
}

func (s *userEndpointStore) FriendAccept(string, string) error  { return s.decisionErr }
func (s *userEndpointStore) FriendDecline(string, string) error { return s.decisionErr }
func (s *userEndpointStore) FriendRemove(string, string) error  { return s.decisionErr }

func (s *userEndpointStore) GrantACL(actor, device, grantee string) (int, error) {
	s.grantActor, s.grantDevice, s.grantGrantee = actor, device, grantee
	return 7, s.grantErr
}

func (s *userEndpointStore) RevokeACLMatch(namespace string, id int, _, _ string) (bool, error) {
	if s.grants[id] != namespace {
		return false, nil
	}
	if s.revoked == nil {
		s.revoked = map[int]bool{}
	}
	s.revoked[id] = true
	return true, nil
}

func userEndpointRequest(t *testing.T, r *Relay, method, path, body, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	return rr
}

func TestUserEndpointsRequireTokenAndRejectPortalToken(t *testing.T) {
	store := &userEndpointStore{}
	r := New(envTokens{"portal-token": "portal", "alice-token": "alice"})
	r.SetPortalNS("portal")
	r.SetAdmin(store)
	paths := []struct {
		method, path, body string
	}{
		{http.MethodGet, "/u/friends", ""},
		{http.MethodPost, "/u/friends/request", `{}`},
		{http.MethodPost, "/u/friends/accept", `{}`},
		{http.MethodPost, "/u/friends/decline", `{}`},
		{http.MethodPost, "/u/friends/remove", `{}`},
		{http.MethodGet, "/u/users/lookup?namespace=bob", ""},
		{http.MethodGet, "/u/shares", ""},
		{http.MethodPost, "/u/shares/grant", `{}`},
		{http.MethodPost, "/u/shares/revoke", `{}`},
	}
	for _, tc := range paths {
		t.Run(tc.path, func(t *testing.T) {
			if rr := userEndpointRequest(t, r, tc.method, tc.path, tc.body, ""); rr.Code != http.StatusUnauthorized {
				t.Fatalf("without token = %d %q", rr.Code, rr.Body.String())
			}
			if rr := userEndpointRequest(t, r, tc.method, tc.path, tc.body, "portal-token"); rr.Code != http.StatusForbidden {
				t.Fatalf("portal token = %d %q", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestUserEndpointErrorTokensAreExact(t *testing.T) {
	tests := []struct {
		name, path, body string
		store            *userEndpointStore
		wantStatus       int
		wantBody         string
	}{
		{
			name: "no such user", path: "/u/friends/request", body: `{"namespace":"bob"}`,
			store: &userEndpointStore{requestErr: ErrNoSuchUser}, wantStatus: http.StatusNotFound, wantBody: "no-such-user",
		},
		{
			name: "no such friend", path: "/u/friends/accept", body: `{"namespace":"bob"}`,
			store: &userEndpointStore{decisionErr: ErrNoSuchFriend}, wantStatus: http.StatusNotFound, wantBody: "no-such-friend",
		},
		{
			name: "not friends", path: "/u/shares/grant", body: `{"device":"d","grantee":"bob","perms":"exec,read"}`,
			store: &userEndpointStore{grantErr: ErrNotFriends}, wantStatus: http.StatusForbidden, wantBody: "not-friends",
		},
		{
			name: "self request", path: "/u/friends/request", body: `{"namespace":"alice"}`,
			store: &userEndpointStore{requestErr: ErrNamespaceConflict}, wantStatus: http.StatusConflict, wantBody: "invalid-friend",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := New(envTokens{"token": "alice"})
			r.SetAdmin(tc.store)
			rr := userEndpointRequest(t, r, http.MethodPost, tc.path, tc.body, "token")
			if rr.Code != tc.wantStatus || rr.Body.String() != tc.wantBody {
				t.Fatalf("response = %d %q, want %d %q", rr.Code, rr.Body.String(), tc.wantStatus, tc.wantBody)
			}
		})
	}
}

// A grant is owner-equivalent, so a "perms" field from an older client or from
// the portal's share form must be accepted and then ignored rather than
// rejected — and it must not narrow what the grantee gets.
func TestUserShareGrantIgnoresAnyRequestedPermissions(t *testing.T) {
	for _, body := range []string{`{"device":"d","grantee":"bob"}`, `{"device":"d","grantee":"bob","perms":"read"}`, `{"device":"d","grantee":"bob","perms":"nonsense"}`} {
		store := &userEndpointStore{}
		r := New(envTokens{"token": "alice"})
		r.SetAdmin(store)
		rr := userEndpointRequest(t, r, http.MethodPost, "/u/shares/grant", body, "token")
		if rr.Code != http.StatusOK || rr.Body.String() != `{"id":7}`+"\n" {
			t.Fatalf("%s -> response = %d %q", body, rr.Code, rr.Body.String())
		}
		if store.grantActor != "alice" || store.grantDevice != "d" || store.grantGrantee != "bob" {
			t.Fatalf("%s -> grant = actor=%q device=%q grantee=%q", body, store.grantActor, store.grantDevice, store.grantGrantee)
		}
	}
}

func TestUserShareRevokeByIDChecksOwner(t *testing.T) {
	store := &userEndpointStore{grants: map[int]string{4: "bob", 5: "alice"}}
	r := New(envTokens{"alice-token": "alice"})
	r.SetAdmin(store)
	other := userEndpointRequest(t, r, http.MethodPost, "/u/shares/revoke", `{"id":4}`, "alice-token")
	if other.Code != http.StatusNotFound || other.Body.String() != "not-found" || store.revoked[4] {
		t.Fatalf("other owner's revoke = %d %q, revoked=%v", other.Code, other.Body.String(), store.revoked)
	}
	own := userEndpointRequest(t, r, http.MethodPost, "/u/shares/revoke", `{"id":5}`, "alice-token")
	if own.Code != http.StatusOK || !store.revoked[5] {
		t.Fatalf("own revoke = %d %q, revoked=%v", own.Code, own.Body.String(), store.revoked)
	}
}

func TestUserAndAdminFriendContracts(t *testing.T) {
	since := time.Date(2026, 8, 20, 3, 4, 5, 0, time.UTC)
	store := &userEndpointStore{
		friends: []Friend{{Namespace: "bob", Status: "accepted", Since: since}},
		users:   map[string]bool{"bob": true},
	}
	r := New(envTokens{"alice-token": "alice"})
	r.SetPortalNS("infra")
	r.SetAdminSecret("secret")
	r.SetAdmin(store)
	listed := userEndpointRequest(t, r, http.MethodGet, "/u/friends", "", "alice-token")
	if listed.Code != http.StatusOK || listed.Body.String() != `{"friends":[{"namespace":"bob","status":"accepted","since":"2026-08-20T03:04:05Z"}]}`+"\n" {
		t.Fatalf("friends response = %d %q", listed.Code, listed.Body.String())
	}
	requested := userEndpointRequest(t, r, http.MethodPost, "/u/friends/request", `{"namespace":"bob"}`, "alice-token")
	if requested.Code != http.StatusOK || store.requestActor != "alice" || store.requestPeer != "bob" || store.requestPortal != "infra" {
		t.Fatalf("request = %d %q, actor=%q peer=%q portal=%q", requested.Code, requested.Body.String(), store.requestActor, store.requestPeer, store.requestPortal)
	}
	adminReq := httptest.NewRequest(http.MethodGet, "/admin/users/lookup?namespace=bob", nil)
	adminReq.Header.Set("X-Admin-Secret", "secret")
	adminRR := httptest.NewRecorder()
	r.Handler().ServeHTTP(adminRR, adminReq)
	if adminRR.Code != http.StatusOK || adminRR.Body.String() != `{"namespace":"bob"}`+"\n" {
		t.Fatalf("admin lookup = %d %q", adminRR.Code, adminRR.Body.String())
	}
	adminReq = httptest.NewRequest(http.MethodPost, "/admin/friends/request", strings.NewReader(`{"namespace":"carol","peer":"bob"}`))
	adminReq.Header.Set("X-Admin-Secret", "secret")
	adminRR = httptest.NewRecorder()
	r.Handler().ServeHTTP(adminRR, adminReq)
	if adminRR.Code != http.StatusOK || store.requestActor != "carol" || store.requestPeer != "bob" {
		t.Fatalf("admin request = %d %q, actor=%q peer=%q", adminRR.Code, adminRR.Body.String(), store.requestActor, store.requestPeer)
	}
}

func TestUserSharesContract(t *testing.T) {
	store := &userEndpointStore{
		given: []map[string]any{{
			"id": 1, "device": "d", "grantee": "bob", "perms": "exec,read",
			"created_at": time.Date(2026, 8, 20, 1, 0, 0, 0, time.UTC),
		}},
		received: []ReceivedShare{{Device: "server", Owner: "carol", Perms: SharedGrant}},
	}
	r := New(envTokens{"token": "alice"})
	r.SetAdmin(store)
	rr := userEndpointRequest(t, r, http.MethodGet, "/u/shares", "", "token")
	// A stale "exec,read" in the database is reported as the one value a grant
	// now has; the field is kept only so existing readers still see something.
	want := `{"given":[{"device":"d","grantee":"bob","id":1,"perms":"full"}],"received":[{"device":"server","owner":"carol","perms":"full"}]}` + "\n"
	if rr.Code != http.StatusOK || rr.Body.String() != want {
		t.Fatalf("shares response = %d %q, want %q", rr.Code, rr.Body.String(), want)
	}
}

func TestUserEndpointWithoutStoreReturnsServiceUnavailable(t *testing.T) {
	r := New(envTokens{"token": "alice"})
	rr := userEndpointRequest(t, r, http.MethodGet, "/u/friends", "", "token")
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("response = %d %q", rr.Code, rr.Body.String())
	}
}

func TestUserLookupMissingBodyIsExact(t *testing.T) {
	r := New(envTokens{"token": "alice"})
	r.SetAdmin(&userEndpointStore{users: map[string]bool{}})
	rr := userEndpointRequest(t, r, http.MethodGet, "/u/users/lookup?namespace=missing", "", "token")
	if rr.Code != http.StatusNotFound || rr.Body.String() != "no-such-user" {
		t.Fatalf("response = %d %q", rr.Code, rr.Body.String())
	}
}

func TestAdminFriendMirrorRequiresSecret(t *testing.T) {
	r := New(envTokens{})
	r.SetAdminSecret("secret")
	r.SetAdmin(&userEndpointStore{})
	for _, path := range []string{"/admin/friends?namespace=alice", "/admin/users/lookup?namespace=bob"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		r.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s response = %d %q", path, rr.Code, rr.Body.String())
		}
	}
}

func TestAdminACLMapsNotFriendsExactly(t *testing.T) {
	store := &userEndpointStore{grantErr: ErrNotFriends}
	// AdminACL calls AddACL; override via a wrapper because noopAdmin's AddACL succeeds.
	wrapped := &adminACLNotFriendsStore{userEndpointStore: store}
	r := New(envTokens{})
	r.SetAdminSecret("secret")
	r.SetAdmin(wrapped)
	req := httptest.NewRequest(http.MethodPost, "/admin/acl", strings.NewReader(`{"namespace":"alice","device":"d","grantee":"bob","perms":"exec,read"}`))
	req.Header.Set("X-Admin-Secret", "secret")
	rr := httptest.NewRecorder()
	r.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden || rr.Body.String() != "not-friends" {
		t.Fatalf("response = %d %q", rr.Code, rr.Body.String())
	}
}

type adminACLNotFriendsStore struct{ *userEndpointStore }

func (*adminACLNotFriendsStore) AddACL(string, string, string) error {
	return ErrNotFriends
}

var _ AdminStore = (*userEndpointStore)(nil)
