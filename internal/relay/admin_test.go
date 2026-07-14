package relay

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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
