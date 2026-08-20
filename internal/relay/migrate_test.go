package relay

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
)

var migrationDriverID uint64

type migrationState struct {
	applied         map[int]bool
	committedBodies []string
	failBody        string
	inTx            bool
	pendingBodies   []string
	pendingVersions []int
}

type migrationDriver struct{ state *migrationState }

func (d migrationDriver) Open(string) (driver.Conn, error) {
	return &migrationConn{state: d.state}, nil
}

type migrationConn struct{ state *migrationState }

func (c *migrationConn) Prepare(query string) (driver.Stmt, error) {
	return migrationStmt{state: c.state, query: query}, nil
}
func (*migrationConn) Close() error { return nil }
func (c *migrationConn) Begin() (driver.Tx, error) {
	if c.state.inTx {
		return nil, errors.New("transaction already active")
	}
	c.state.inTx = true
	c.state.pendingBodies = nil
	c.state.pendingVersions = nil
	return migrationTx{state: c.state}, nil
}

type migrationTx struct{ state *migrationState }

func (tx migrationTx) Commit() error {
	for _, version := range tx.state.pendingVersions {
		tx.state.applied[version] = true
	}
	tx.state.committedBodies = append(tx.state.committedBodies, tx.state.pendingBodies...)
	tx.state.inTx = false
	return nil
}
func (tx migrationTx) Rollback() error {
	tx.state.pendingBodies = nil
	tx.state.pendingVersions = nil
	tx.state.inTx = false
	return nil
}

type migrationStmt struct {
	state *migrationState
	query string
}

func (migrationStmt) Close() error  { return nil }
func (migrationStmt) NumInput() int { return -1 }
func (s migrationStmt) Exec(args []driver.Value) (driver.Result, error) {
	if strings.Contains(s.query, "CREATE TABLE IF NOT EXISTS schema_migrations") {
		return driver.RowsAffected(0), nil
	}
	if strings.Contains(s.query, "INSERT INTO schema_migrations") {
		s.state.pendingVersions = append(s.state.pendingVersions, int(args[0].(int64)))
		return driver.RowsAffected(1), nil
	}
	if s.query == s.state.failBody {
		return nil, errors.New("injected migration failure")
	}
	if !s.state.inTx {
		return nil, errors.New("migration body executed outside transaction")
	}
	s.state.pendingBodies = append(s.state.pendingBodies, s.query)
	return driver.RowsAffected(0), nil
}
func (s migrationStmt) Query([]driver.Value) (driver.Rows, error) {
	if !strings.Contains(s.query, "SELECT version FROM schema_migrations") {
		return nil, fmt.Errorf("unexpected query: %s", s.query)
	}
	values := make([][]driver.Value, 0, len(s.state.applied))
	for version := 1; version < 1000; version++ {
		if s.state.applied[version] {
			values = append(values, []driver.Value{int64(version)})
		}
	}
	return &migrationRows{values: values}, nil
}

type migrationRows struct {
	values [][]driver.Value
	index  int
}

func (*migrationRows) Columns() []string { return []string{"version"} }
func (*migrationRows) Close() error      { return nil }
func (r *migrationRows) Next(dest []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.index])
	r.index++
	return nil
}

func newMigrationTestDB(t *testing.T, state *migrationState) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("wanctl_migration_test_%d", atomic.AddUint64(&migrationDriverID, 1))
	sql.Register(name, migrationDriver{state: state})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func migrationTestFS() fstest.MapFS {
	return fstest.MapFS{
		"migrations/002_second.sql": {Data: []byte("second")},
		"migrations/001_first.sql":  {Data: []byte("first")},
	}
}

func TestRunMigrationsAppliesInOrder(t *testing.T) {
	state := &migrationState{applied: map[int]bool{}}
	if err := runMigrations(newMigrationTestDB(t, state), migrationTestFS()); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(state.committedBodies, ","), "first,second"; got != want {
		t.Fatalf("committed migrations = %q, want %q", got, want)
	}
	if !state.applied[1] || !state.applied[2] {
		t.Fatalf("applied versions = %#v", state.applied)
	}
}

func TestRunMigrationsSkipsAppliedVersions(t *testing.T) {
	state := &migrationState{applied: map[int]bool{1: true}}
	if err := runMigrations(newMigrationTestDB(t, state), migrationTestFS()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(state.committedBodies, ","); got != "second" {
		t.Fatalf("committed migrations = %q, want second", got)
	}
}

func TestRunMigrationsRollsBackFailedVersion(t *testing.T) {
	state := &migrationState{applied: map[int]bool{}, failBody: "second"}
	err := runMigrations(newMigrationTestDB(t, state), migrationTestFS())
	if err == nil || !strings.Contains(err.Error(), "002_second.sql") {
		t.Fatalf("migration error = %v", err)
	}
	if !state.applied[1] || state.applied[2] {
		t.Fatalf("failed migration was recorded: %#v", state.applied)
	}
	if got := strings.Join(state.committedBodies, ","); got != "first" {
		t.Fatalf("committed migrations = %q, want first", got)
	}
}
