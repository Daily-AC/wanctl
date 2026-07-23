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
	if strings.Contains(s.query, "FROM users") {
		return &adminRows{
			columns: []string{"namespace"},
			values:  [][]driver.Value{{"renjinxi"}, {"***REMOVED***"}},
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
	case "***REMOVED***":
		rows = append(rows, []driver.Value{"zyl", "fp-zyl", seen, "***REMOVED***", false, ""})
	case "renjinxi":
		rows = append(rows, []driver.Value{"book", "fp-book", seen, "renjinxi", false, ""})
		rows = append(rows, []driver.Value{"zyl", "fp-zyl", seen, "***REMOVED***", true, "exec"})
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
	byIdentity  map[string]string
	byNamespace map[string]string
	inserts     int
}

type resolveUserDriver struct{ state *resolveUserState }

func (d resolveUserDriver) Open(string) (driver.Conn, error) {
	return resolveUserConn{state: d.state}, nil
}

type resolveUserConn struct{ state *resolveUserState }

func (c resolveUserConn) Prepare(query string) (driver.Stmt, error) {
	return resolveUserStmt{state: c.state, query: query}, nil
}
func (resolveUserConn) Close() error              { return nil }
func (resolveUserConn) Begin() (driver.Tx, error) { return nil, errors.New("transactions unsupported") }

type resolveUserStmt struct {
	state *resolveUserState
	query string
}

func (resolveUserStmt) Close() error  { return nil }
func (resolveUserStmt) NumInput() int { return -1 }
func (resolveUserStmt) Exec([]driver.Value) (driver.Result, error) {
	return nil, errors.New("exec unsupported")
}
func (s resolveUserStmt) Query(args []driver.Value) (driver.Rows, error) {
	if strings.Contains(s.query, "SELECT namespace FROM users WHERE feishu_open_id") {
		identity := args[0].(string)
		if ns, ok := s.state.byIdentity[identity]; ok {
			return &adminRows{columns: []string{"namespace"}, values: [][]driver.Value{{ns}}}, nil
		}
		return &adminRows{columns: []string{"namespace"}}, nil
	}
	if strings.Contains(s.query, "INSERT INTO users") {
		s.state.inserts++
		identity, ns := args[0].(string), args[1].(string)
		if owner, exists := s.state.byNamespace[ns]; exists {
			if strings.Contains(s.query, "DO UPDATE") {
				delete(s.state.byIdentity, owner)
				s.state.byIdentity[identity] = ns
				s.state.byNamespace[ns] = identity
				return &adminRows{columns: []string{"namespace"}, values: [][]driver.Value{{ns}}}, nil
			}
			if strings.Contains(s.query, "DO NOTHING") {
				return &adminRows{columns: []string{"namespace"}}, nil
			}
			return nil, errors.New("unhandled namespace conflict")
		}
		s.state.byIdentity[identity] = ns
		s.state.byNamespace[ns] = identity
		return &adminRows{columns: []string{"namespace"}, values: [][]driver.Value{{ns}}}, nil
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
	state := &resolveUserState{
		byIdentity:  map[string]string{"alice@old.example": "alice"},
		byNamespace: map[string]string{"alice": "alice@old.example"},
	}
	p := newResolveUserTestPGStore(t, state)

	if ns, err := p.ResolveUser("alice@new.example"); !errors.Is(err, ErrNamespaceConflict) {
		t.Fatalf("ResolveUser = %q, %v; want ErrNamespaceConflict", ns, err)
	}
	if got := state.byNamespace["alice"]; got != "alice@old.example" {
		t.Fatalf("namespace owner changed to %q", got)
	}
}

type namespaceConflictAdmin struct{ noopAdmin }

func (*namespaceConflictAdmin) ResolveUser(string) (string, error) {
	return "", fmt.Errorf("%w: %q", ErrNamespaceConflict, "alice")
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

func TestPGStoreResolveUserIsStableForImmutableIdentity(t *testing.T) {
	state := &resolveUserState{
		byIdentity:  map[string]string{},
		byNamespace: map[string]string{},
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

func TestPGStoreListDevicesIncludesOwnedAndSharedACLDevices(t *testing.T) {
	p := newAdminTestPGStore(t)

	ownerView, err := p.ListDevices("***REMOVED***")
	if err != nil {
		t.Fatal(err)
	}
	if len(ownerView) != 1 {
		t.Fatalf("owner view should only contain owned devices, got %+v", ownerView)
	}
	if ownerView[0]["name"] != "zyl" || ownerView[0]["owner"] != "***REMOVED***" || ownerView[0]["shared"] != false {
		t.Fatalf("owner row missing owner/shared fields: %+v", ownerView[0])
	}

	granteeView, err := p.ListDevices("renjinxi")
	if err != nil {
		t.Fatal(err)
	}
	if len(granteeView) != 2 {
		t.Fatalf("grantee view should contain own + granted devices, got %+v", granteeView)
	}
	shared := granteeView[1]
	if shared["name"] != "zyl" || shared["owner"] != "***REMOVED***" || shared["shared"] != true || shared["perms"] != "exec" {
		t.Fatalf("shared row missing expected fields: %+v", shared)
	}
}

func TestPGStoreListUsersReturnsNamespaces(t *testing.T) {
	p := newAdminTestPGStore(t)

	namespaces, err := p.ListUsers()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"renjinxi", "***REMOVED***"}
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
	r.SetAdmin(&adminUsersStore{users: []string{"renjinxi", "***REMOVED***"}})
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
	want := []string{"renjinxi", "***REMOVED***"}
	if !reflect.DeepEqual(out.Namespaces, want) {
		t.Fatalf("namespaces = %+v, want %+v", out.Namespaces, want)
	}
}

func TestAdminDevicesUsesOwnerNamespaceForSharedLiveness(t *testing.T) {
	r := New(envTokens{})
	r.SetAdminSecret("secret")
	r.SetAdmin(&adminUsersStore{devices: []map[string]any{
		{"name": "zyl", "owner": "***REMOVED***", "shared": true, "perms": "exec"},
	}})
	r.mu.Lock()
	r.agents["***REMOVED***/zyl"] = &agentConn{}
	r.mu.Unlock()
	h := r.Handler()

	req := httptest.NewRequest("GET", "/admin/devices?namespace=renjinxi", nil)
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
