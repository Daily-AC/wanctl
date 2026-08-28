package relay

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func init() {
	sql.Register("wanctl_admin_test", adminTestDriver{})
}

type adminTestDriver struct{}

func (adminTestDriver) Open(string) (driver.Conn, error) { return adminTestConn{}, nil }

type adminTestConn struct{}

func (adminTestConn) Prepare(query string) (driver.Stmt, error) {
	return adminTestStmt{query: query}, nil
}
func (adminTestConn) Close() error              { return nil }
func (adminTestConn) Begin() (driver.Tx, error) { return nil, errors.New("transactions unsupported") }

type adminTestStmt struct {
	query string
}

func (adminTestStmt) Close() error  { return nil }
func (adminTestStmt) NumInput() int { return -1 }
func (s adminTestStmt) Exec([]driver.Value) (driver.Result, error) {
	return nil, errors.New("exec unsupported")
}
func (s adminTestStmt) Query(args []driver.Value) (driver.Rows, error) {
	updated := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	if strings.Contains(s.query, "INSERT INTO device_lark_approval") {
		if !strings.Contains(s.query, "ON CONFLICT (namespace, device) DO UPDATE") {
			return nil, errors.New("lark upsert must use namespace/device conflict target")
		}
		want := []driver.Value{"alice", "legion", true, false, "alice@example.com"}
		if !reflect.DeepEqual(args, want) {
			return nil, fmt.Errorf("lark upsert args = %#v, want %#v", args, want)
		}
		return &adminRows{
			columns: []string{"namespace", "device", "approval_enabled", "pairing_from_card", "notify_email", "updated_at"},
			values:  [][]driver.Value{{"alice", "legion", true, false, "alice@example.com", updated}},
		}, nil
	}
	if strings.Contains(s.query, "FROM device_lark_approval") {
		if len(args) != 1 || args[0] != "alice" || !strings.Contains(s.query, "WHERE namespace = $1") {
			return nil, fmt.Errorf("lark list is not namespace-parameterized: query=%q args=%#v", s.query, args)
		}
		return &adminRows{
			columns: []string{"namespace", "device", "approval_enabled", "pairing_from_card", "notify_email", "updated_at"},
			values:  [][]driver.Value{{"alice", "legion", true, false, "alice@example.com", updated}},
		}, nil
	}
	if strings.Contains(s.query, "FROM users") {
		return &adminRows{
			columns: []string{"namespace"},
			values:  [][]driver.Value{{"alice"}, {"bob"}},
		}, nil
	}
	if strings.Contains(s.query, "SELECT perms FROM acl") {
		if !strings.Contains(s.query, "revoked_at IS NULL") {
			return nil, errors.New("ACL lookup must exclude revoked grants")
		}
		if len(args) == 3 && args[0] == "owner" && args[1] == "home-pc" && args[2] == "reader" {
			return &adminRows{columns: []string{"perms"}, values: [][]driver.Value{{"read"}}}, nil
		}
		return &adminRows{columns: []string{"perms"}}, nil
	}
	if !strings.Contains(s.query, "UNION ALL") || !strings.Contains(s.query, "revoked_at IS NULL") {
		return nil, errors.New("ListDevices query must include own devices, ACL devices, and revoked filter")
	}
	ns := args[0].(string)
	seen := time.Date(2026, 7, 14, 9, 0, 0, 0, time.UTC)
	rows := [][]driver.Value{}
	switch ns {
	case "bob":
		rows = append(rows, []driver.Value{"devbox", "fp-devbox", seen, "bob", false, ""})
	case "alice":
		rows = append(rows, []driver.Value{"book", "fp-book", seen, "alice", false, ""})
		rows = append(rows, []driver.Value{"devbox", "fp-devbox", seen, "bob", true, "exec"})
	}
	return &adminRows{
		columns: []string{"name", "fingerprint", "last_seen", "owner", "shared", "perms"},
		values:  rows,
	}, nil
}

type adminRows struct {
	columns []string
	values  [][]driver.Value
	i       int
}

func (r *adminRows) Columns() []string { return r.columns }
func (r *adminRows) Close() error      { return nil }
func (r *adminRows) Next(dest []driver.Value) error {
	if r.i >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.i])
	r.i++
	return nil
}

func newAdminTestPGStore(t *testing.T) *PGStore {
	t.Helper()
	db, err := sql.Open("wanctl_admin_test", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &PGStore{db: db}
}

var resolveUserDriverID uint64

type resolveUserState struct {
	byIdentity  map[string]identityRecord
	byNamespace map[string]string
	invites     map[int]*inviteRecord
	inserts     int
}

type identityRecord struct {
	namespace string
	role      string
}

type inviteRecord struct {
	codeHash string
	login    string
	usedBy   string
}

func identityKey(provider, subject string) string { return provider + "\x00" + subject }

type resolveUserDriver struct{ state *resolveUserState }

func (d resolveUserDriver) Open(string) (driver.Conn, error) {
	return resolveUserConn{state: d.state}, nil
}

type resolveUserConn struct{ state *resolveUserState }

func (c resolveUserConn) Prepare(query string) (driver.Stmt, error) {
	return resolveUserStmt{state: c.state, query: query}, nil
}
func (resolveUserConn) Close() error { return nil }
func (c resolveUserConn) Begin() (driver.Tx, error) {
	return resolveUserTx{}, nil
}

type resolveUserTx struct{}

func (resolveUserTx) Commit() error   { return nil }
func (resolveUserTx) Rollback() error { return nil }

type resolveUserStmt struct {
	state *resolveUserState
	query string
}

func (resolveUserStmt) Close() error  { return nil }
func (resolveUserStmt) NumInput() int { return -1 }
func (s resolveUserStmt) Exec(args []driver.Value) (driver.Result, error) {
	if strings.Contains(s.query, "pg_advisory_xact_lock") {
		return driver.RowsAffected(0), nil
	}
	if strings.Contains(s.query, "UPDATE invites SET used_at") {
		id, namespace := int(args[0].(int64)), args[1].(string)
		invite := s.state.invites[id]
		if invite == nil || invite.usedBy != "" {
			return driver.RowsAffected(0), nil
		}
		invite.usedBy = namespace
		return driver.RowsAffected(1), nil
	}
	return nil, fmt.Errorf("unexpected exec: %s", s.query)
}
func (s resolveUserStmt) Query(args []driver.Value) (driver.Rows, error) {
	if strings.Contains(s.query, "SELECT namespace, role FROM users") {
		key := identityKey(args[0].(string), args[1].(string))
		if record, ok := s.state.byIdentity[key]; ok {
			return &adminRows{
				columns: []string{"namespace", "role"},
				values:  [][]driver.Value{{record.namespace, record.role}},
			}, nil
		}
		return &adminRows{columns: []string{"namespace", "role"}}, nil
	}
	if strings.Contains(s.query, "INSERT INTO users") {
		s.state.inserts++
		provider, subject := args[0].(string), args[1].(string)
		ns, role := args[2].(string), args[4].(string)
		key := identityKey(provider, subject)
		if _, exists := s.state.byNamespace[ns]; exists {
			return &adminRows{columns: []string{"namespace", "role"}}, nil
		}
		s.state.byIdentity[key] = identityRecord{namespace: ns, role: role}
		s.state.byNamespace[ns] = key
		return &adminRows{
			columns: []string{"namespace", "role"},
			values:  [][]driver.Value{{ns, role}},
		}, nil
	}
	if strings.Contains(s.query, "SELECT EXISTS") {
		hasAdmin := false
		for _, record := range s.state.byIdentity {
			if record.role == "admin" {
				hasAdmin = true
				break
			}
		}
		return &adminRows{columns: []string{"exists"}, values: [][]driver.Value{{hasAdmin}}}, nil
	}
	if strings.Contains(s.query, "WHERE code_hash") {
		hash := args[0].(string)
		for id, invite := range s.state.invites {
			if invite.codeHash == hash && invite.usedBy == "" {
				return &adminRows{columns: []string{"id"}, values: [][]driver.Value{{int64(id)}}}, nil
			}
		}
		return &adminRows{columns: []string{"id"}}, nil
	}
	if strings.Contains(s.query, "lower(github_login)") {
		login := strings.ToLower(args[0].(string))
		for id, invite := range s.state.invites {
			if strings.ToLower(invite.login) == login && invite.usedBy == "" {
				return &adminRows{columns: []string{"id"}, values: [][]driver.Value{{int64(id)}}}, nil
			}
		}
		return &adminRows{columns: []string{"id"}}, nil
	}
	return nil, fmt.Errorf("unexpected query: %s", s.query)
}

func newResolveUserTestPGStore(t *testing.T, state *resolveUserState) *PGStore {
	t.Helper()
	name := fmt.Sprintf("wanctl_resolve_user_test_%d", atomic.AddUint64(&resolveUserDriverID, 1))
	sql.Register(name, resolveUserDriver{state: state})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &PGStore{db: db}
}

func TestPGStoreResolveUserDoesNotReassignNamespace(t *testing.T) {
	owner := identityKey("header", "alice@old.example")
	state := &resolveUserState{
		byIdentity:  map[string]identityRecord{owner: {namespace: "alice", role: "user"}},
		byNamespace: map[string]string{"alice": owner},
		invites:     map[int]*inviteRecord{},
	}
	p := newResolveUserTestPGStore(t, state)

	if ns, err := p.ResolveUser("alice@new.example"); !errors.Is(err, ErrNamespaceConflict) {
		t.Fatalf("ResolveUser = %q, %v; want ErrNamespaceConflict", ns, err)
	}
	if got := state.byNamespace["alice"]; got != owner {
		t.Fatalf("namespace owner changed to %q", got)
	}
}

type namespaceConflictAdmin struct{ noopAdmin }

func (*namespaceConflictAdmin) ResolveIdentity(string, string, string, string, string, string) (string, string, error) {
	return "", "", fmt.Errorf("%w: %q", ErrNamespaceConflict, "alice")
}

func TestAdminResolveUserReturnsConflict(t *testing.T) {
	r := New(envTokens{})
	r.SetAdminSecret("secret")
	r.SetAdmin(&namespaceConflictAdmin{})
	req := httptest.NewRequest("POST", "/admin/resolve-user", strings.NewReader(`{"identity":"alice@new.example"}`))
	req.Header.Set("X-Admin-Secret", "secret")
	rr := httptest.NewRecorder()

	r.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "already owned") {
		t.Fatalf("conflict reason missing from response: %q", rr.Body.String())
	}
}

func TestAdminACLRejectsMissingPermissions(t *testing.T) {
	r := New(envTokens{})
	r.SetAdminSecret("secret")
	r.SetAdmin(&noopAdmin{})
	req := httptest.NewRequest(http.MethodPost, "/admin/acl", strings.NewReader(
		`{"namespace":"alice","device":"devbox","grantee":"bob"}`))
	req.Header.Set("X-Admin-Secret", "secret")
	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %q", rec.Code, rec.Body.String())
	}
}

type resolveIdentityAdmin struct {
	noopAdmin
	provider, subject, reservedNS string
	err                           error
}

func (a *resolveIdentityAdmin) ResolveIdentity(provider, subject, _, _, _, reservedNS string) (string, string, error) {
	a.provider, a.subject, a.reservedNS = provider, subject, reservedNS
	if a.err != nil {
		return "", "", a.err
	}
	return "legacy", "user", nil
}

func TestAdminResolveUserLegacyRequestIncludesRole(t *testing.T) {
	admin := &resolveIdentityAdmin{}
	r := New(envTokens{})
	r.SetAdminSecret("secret")
	r.SetAdmin(admin)
	r.SetPortalNS("portal")
	req := httptest.NewRequest("POST", "/admin/resolve-user", strings.NewReader(`{"identity":"legacy@example.com"}`))
	req.Header.Set("X-Admin-Secret", "secret")
	rr := httptest.NewRecorder()

	r.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	if admin.provider != "header" || admin.subject != "legacy@example.com" {
		t.Fatalf("resolved provider/subject = %q/%q", admin.provider, admin.subject)
	}
	if admin.reservedNS != "portal" {
		t.Fatalf("reserved namespace = %q, want portal", admin.reservedNS)
	}
	var body map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["namespace"] != "legacy" || body["role"] != "user" {
		t.Fatalf("response = %#v", body)
	}
}

func TestAdminResolveUserPendingInviteBodyIsExact(t *testing.T) {
	r := New(envTokens{})
	r.SetAdminSecret("secret")
	r.SetAdmin(&resolveIdentityAdmin{err: ErrPendingInvite})
	req := httptest.NewRequest("POST", "/admin/resolve-user", strings.NewReader(
		`{"provider":"github","subject":"123","login":"octocat"}`,
	))
	req.Header.Set("X-Admin-Secret", "secret")
	rr := httptest.NewRecorder()

	r.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden || rr.Body.String() != "pending-invite" {
		t.Fatalf("response = %d %q; want 403 pending-invite", rr.Code, rr.Body.String())
	}
}

func TestAdminResolveUserWithoutStoreReturnsServiceUnavailable(t *testing.T) {
	r := New(envTokens{})
	r.SetAdminSecret("secret")
	req := httptest.NewRequest("POST", "/admin/resolve-user", strings.NewReader(`{"identity":"legacy@example.com"}`))
	req.Header.Set("X-Admin-Secret", "secret")
	rr := httptest.NewRecorder()

	r.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rr.Code)
	}
}

type invitesAdmin struct {
	noopAdmin
	invites   []Invite
	revokedID int
}

func (a *invitesAdmin) CreateInvite(login string) (Invite, string, error) {
	if login == "" {
		return Invite{ID: 3, HasCode: true}, "winv_secret", nil
	}
	return Invite{ID: 4, GitHubLogin: login}, "", nil
}

func (a *invitesAdmin) ListInvites() ([]Invite, error) { return a.invites, nil }

func (a *invitesAdmin) RevokeInvite(id int) (bool, error) {
	a.revokedID = id
	return id == 3, nil
}

func TestAdminInvitesContractAndNoHashLeak(t *testing.T) {
	created := time.Date(2026, 8, 20, 1, 2, 3, 0, time.UTC)
	admin := &invitesAdmin{invites: []Invite{{
		ID: 3, CreatedAt: created, UsedByNamespace: "", HasCode: true,
	}}}
	r := New(envTokens{})
	r.SetAdminSecret("secret")
	r.SetAdmin(admin)
	h := r.Handler()

	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-Admin-Secret", "secret")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	if rr := request("POST", "/admin/invites", `{}`); rr.Code != http.StatusOK || rr.Body.String() != "{\"code\":\"winv_secret\",\"id\":3}\n" {
		t.Fatalf("code invite response = %d %q", rr.Code, rr.Body.String())
	}
	if rr := request("POST", "/admin/invites", `{"github_login":"monalisa"}`); rr.Code != http.StatusOK || rr.Body.String() != "{\"github_login\":\"monalisa\",\"id\":4}\n" {
		t.Fatalf("login invite response = %d %q", rr.Code, rr.Body.String())
	}
	rr := request("GET", "/admin/invites", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list response = %d %q", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "hash") || strings.Contains(rr.Body.String(), "winv_secret") {
		t.Fatalf("invite list leaked secret material: %s", rr.Body.String())
	}
	var listed []map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0]["has_code"] != true || listed[0]["used_at"] != nil {
		t.Fatalf("invite list = %#v", listed)
	}

	if rr := request("POST", "/admin/invites/revoke", `{"id":3}`); rr.Code != http.StatusOK || admin.revokedID != 3 {
		t.Fatalf("revoke response = %d, id = %d", rr.Code, admin.revokedID)
	}
	if rr := request("POST", "/admin/invites/revoke", `{"id":99}`); rr.Code != http.StatusNotFound {
		t.Fatalf("missing revoke response = %d, want 404", rr.Code)
	}
}

func TestPGStoreResolveUserIsStableForImmutableIdentity(t *testing.T) {
	state := &resolveUserState{
		byIdentity:  map[string]identityRecord{},
		byNamespace: map[string]string{},
		invites:     map[int]*inviteRecord{},
	}
	p := newResolveUserTestPGStore(t, state)

	first, err := p.ResolveUser("stable@example.com")
	if err != nil {
		t.Fatalf("first ResolveUser: %v", err)
	}
	second, err := p.ResolveUser("stable@example.com")
	if err != nil {
		t.Fatalf("second ResolveUser: %v", err)
	}
	if first != "stable" || second != first {
		t.Fatalf("namespaces = %q, %q; want stable", first, second)
	}
	if state.inserts != 1 {
		t.Fatalf("insert count = %d, want 1", state.inserts)
	}
}

func TestPGStoreGitHubAdmission(t *testing.T) {
	state := &resolveUserState{
		byIdentity:  map[string]identityRecord{},
		byNamespace: map[string]string{},
		invites: map[int]*inviteRecord{
			1: {codeHash: HashToken("winv_once")},
			2: {login: "PreRecorded"},
		},
	}
	p := newResolveUserTestPGStore(t, state)

	ns, role, err := p.ResolveIdentity("github", "100", "FirstUser", "First User", "", "portal")
	if err != nil || ns != "firstuser" || role != "admin" {
		t.Fatalf("first GitHub identity = %q, %q, %v; want firstuser, admin", ns, role, err)
	}
	if _, _, err := p.ResolveIdentity("github", "101", "uninvited", "", "", "portal"); !errors.Is(err, ErrPendingInvite) {
		t.Fatalf("second GitHub identity error = %v, want ErrPendingInvite", err)
	}

	ns, role, err = p.ResolveIdentity("github", "102", "code-user", "", "winv_once", "portal")
	if err != nil || ns != "code-user" || role != "user" {
		t.Fatalf("code invite identity = %q, %q, %v", ns, role, err)
	}
	if got := state.invites[1].usedBy; got != "code-user" {
		t.Fatalf("code invite used_by = %q", got)
	}
	if _, _, err := p.ResolveIdentity("github", "103", "reuse", "", "winv_once", "portal"); !errors.Is(err, ErrPendingInvite) {
		t.Fatalf("reused code error = %v, want ErrPendingInvite", err)
	}

	ns, role, err = p.ResolveIdentity("github", "104", "prerecorded", "", "", "portal")
	if err != nil || ns != "prerecorded" || role != "user" {
		t.Fatalf("login invite identity = %q, %q, %v", ns, role, err)
	}
	if got := state.invites[2].usedBy; got != "prerecorded" {
		t.Fatalf("login invite used_by = %q", got)
	}
}

func TestPGStoreResolveIdentityRejectsReservedNamespace(t *testing.T) {
	state := &resolveUserState{
		byIdentity:  map[string]identityRecord{},
		byNamespace: map[string]string{},
		invites:     map[int]*inviteRecord{},
	}
	p := newResolveUserTestPGStore(t, state)
	for _, tc := range []struct {
		provider string
		subject  string
		login    string
	}{
		{provider: "header", subject: "portal@example.com"},
		{provider: "github", subject: "200", login: "Portal"},
	} {
		if _, _, err := p.ResolveIdentity(tc.provider, tc.subject, tc.login, "", "", "portal"); !errors.Is(err, ErrNamespaceConflict) {
			t.Fatalf("ResolveIdentity(%q) error = %v, want ErrNamespaceConflict", tc.provider, err)
		}
	}
}

func TestPGStoreListDevicesIncludesOwnedAndSharedACLDevices(t *testing.T) {
	p := newAdminTestPGStore(t)

	ownerView, err := p.ListDevices("bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(ownerView) != 1 {
		t.Fatalf("owner view should only contain owned devices, got %+v", ownerView)
	}
	if ownerView[0]["name"] != "devbox" || ownerView[0]["owner"] != "bob" || ownerView[0]["shared"] != false {
		t.Fatalf("owner row missing owner/shared fields: %+v", ownerView[0])
	}

	granteeView, err := p.ListDevices("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(granteeView) != 2 {
		t.Fatalf("grantee view should contain own + granted devices, got %+v", granteeView)
	}
	shared := granteeView[1]
	if shared["name"] != "devbox" || shared["owner"] != "bob" || shared["shared"] != true || shared["perms"] != "exec" {
		t.Fatalf("shared row missing expected fields: %+v", shared)
	}
}

func TestPGStoreListUsersReturnsNamespaces(t *testing.T) {
	p := newAdminTestPGStore(t)

	namespaces, err := p.ListUsers()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alice", "bob"}
	if !reflect.DeepEqual(namespaces, want) {
		t.Fatalf("namespaces = %+v, want %+v", namespaces, want)
	}
}

func TestPGStoreACLPermsReturnsLiveGrant(t *testing.T) {
	p := newAdminTestPGStore(t)
	perms, ok := p.ACLPerms("reader", "owner", "home-pc")
	if !ok || perms != "read" {
		t.Fatalf("ACLPerms = %q, %v", perms, ok)
	}
	if _, ok := p.ACLPerms("other", "owner", "home-pc"); ok {
		t.Fatal("missing grant should not be allowed")
	}
}

func TestPGStoreAddACLRejectsInvalidPermissionsBeforeDatabase(t *testing.T) {
	p := newAdminTestPGStore(t)
	if err := p.AddACL("owner", "home-pc", "reader", "read,unknown"); err == nil {
		t.Fatal("invalid permissions were accepted")
	}
}

type adminUsersStore struct {
	noopAdmin
	devices []map[string]any
	users   []string
}

func (a *adminUsersStore) ListDevices(string) ([]map[string]any, error) {
	out := make([]map[string]any, len(a.devices))
	for i, d := range a.devices {
		cp := map[string]any{}
		for k, v := range d {
			cp[k] = v
		}
		out[i] = cp
	}
	return out, nil
}

func (a *adminUsersStore) ListUsers() ([]string, error) { return a.users, nil }

func TestAdminUsersSecretGated(t *testing.T) {
	r := New(envTokens{})
	r.SetAdminSecret("secret")
	r.SetAdmin(&adminUsersStore{users: []string{"alice", "bob"}})
	h := r.Handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/admin/users", nil))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("without secret: want 403, got %d", rr.Code)
	}

	req := httptest.NewRequest("GET", "/admin/users", nil)
	req.Header.Set("X-Admin-Secret", "secret")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("with secret: want 200, got %d body %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Namespaces []string `json:"namespaces"`
	}
	json.NewDecoder(rr.Body).Decode(&out)
	want := []string{"alice", "bob"}
	if !reflect.DeepEqual(out.Namespaces, want) {
		t.Fatalf("namespaces = %+v, want %+v", out.Namespaces, want)
	}
}

func TestAdminDevicesUsesOwnerNamespaceForSharedLiveness(t *testing.T) {
	r := New(envTokens{})
	r.SetAdminSecret("secret")
	r.SetAdmin(&adminUsersStore{devices: []map[string]any{
		{"name": "devbox", "owner": "bob", "shared": true, "perms": "exec"},
	}})
	r.mu.Lock()
	r.agents["bob/devbox"] = &agentConn{}
	r.mu.Unlock()
	h := r.Handler()

	req := httptest.NewRequest("GET", "/admin/devices?namespace=alice", nil)
	req.Header.Set("X-Admin-Secret", "secret")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d body %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Devices []map[string]any `json:"devices"`
	}
	json.NewDecoder(rr.Body).Decode(&out)
	if len(out.Devices) != 1 || out.Devices[0]["online"] != true {
		t.Fatalf("shared device should be online via owner namespace, got %+v", out.Devices)
	}
}
