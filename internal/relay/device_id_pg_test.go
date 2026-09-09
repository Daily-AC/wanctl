package relay

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"testing/fstest"

	"wanctl/internal/client"
	"wanctl/internal/transport"

	"github.com/google/uuid"
)

// Opt-in against a disposable PostgreSQL server. Each test owns an isolated schema.
func deviceIDTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("WANCTL_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("set WANCTL_TEST_POSTGRES to run the real PostgreSQL migration test")
	}
	base, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "device_id_" + randHex(8)
	if _, err := base.Exec("CREATE SCHEMA " + schema); err != nil {
		base.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { base.Exec("DROP SCHEMA " + schema + " CASCADE"); base.Close() })
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestDeviceIDPostgresMigrationAndDuplicateNames(t *testing.T) {
	db := deviceIDTestDB(t)
	oldFS := fstest.MapFS{}
	paths, _ := fs.Glob(migrationFiles, "migrations/*.sql")
	for _, path := range paths {
		if path == "migrations/007_device_id.sql" {
			continue
		}
		b, _ := fs.ReadFile(migrationFiles, path)
		oldFS[path] = &fstest.MapFile{Data: b}
	}
	if err := runMigrations(db, oldFS); err != nil {
		t.Fatal(err)
	}
	fp := transport.Fingerprint([]byte("original certificate"))
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO devices(owner_namespace,name,fingerprint,alias) VALUES ('alice','fcs-ubuntu',$1,'北京电脑虚拟机')`, fp)
	exec(`INSERT INTO acl(owner_namespace,device,grantee_namespace,perms) VALUES ('alice','fcs-ubuntu','bob','read')`)
	exec(`INSERT INTO audit(namespace,device,event) VALUES ('alice','fcs-ubuntu','dial')`)
	exec(`INSERT INTO device_lark_approval(namespace,device,notify_email) VALUES ('alice','fcs-ubuntu','test@example.com')`)
	exec(`INSERT INTO device_notify(namespace,device,enabled) VALUES ('alice','fcs-ubuntu',true)`)
	exec(`INSERT INTO notify_health(namespace,device,attempted_at,result) VALUES ('alice','fcs-ubuntu',now(),'ok')`)
	if err := runMigrations(db, migrationFiles); err != nil {
		t.Fatal(err)
	}
	p := &PGStore{db: db}
	first, second := uuid.NewString(), uuid.NewString()
	otherFP := transport.Fingerprint([]byte("other certificate"))
	// A different fingerprint with the same name must not inherit the old ACL.
	if created, err := p.RegisterDevice("alice", second, "fcs-ubuntu", otherFP); err != nil || !created {
		t.Fatalf("new duplicate: %v %v", created, err)
	}
	if _, err := p.ResolveDeviceTargetStrict("alice", "fcs-ubuntu"); err == nil {
		t.Fatal("legacy and upgraded same-name devices must also be ambiguous")
	}
	if _, ok := p.ACLGrant("bob", "alice", second); ok {
		t.Fatal("duplicate inherited old sharing")
	}
	if created, err := p.RegisterDevice("alice", first, "fcs-ubuntu", fp); err != nil || created {
		t.Fatalf("migration: %v %v", created, err)
	}
	for _, table := range []string{"acl", "audit", "device_lark_approval", "device_notify", "notify_health"} {
		var count int
		if err := db.QueryRow("SELECT count(*) FROM "+table+" WHERE device=$1", first).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s association: %d %v", table, count, err)
		}
	}
	if _, ok := p.ACLGrant("bob", "alice", first); !ok {
		t.Fatal("migrated ACL")
	}
	rows, err := p.ListDevices("alice")
	if err != nil || len(rows) != 2 {
		t.Fatalf("two devices: %+v %v", rows, err)
	}
	for _, row := range rows {
		if row["display_name"] != "fcs-ubuntu" || row["device_id"] == "" {
			t.Fatalf("device data: %+v", row)
		}
	}
	if _, err := p.ResolveDeviceTargetStrict("alice", "fcs-ubuntu"); err == nil {
		t.Fatal("ambiguous name resolved")
	}
	if id, err := p.ResolveDeviceTargetStrict("alice", first); err != nil || id != first {
		t.Fatalf("exact ID: %s %v", id, err)
	}
	if _, err := p.SetDeviceAlias("alice", second, "北京电脑虚拟机"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ResolveDeviceTargetStrict("alice", "北京电脑虚拟机"); err == nil {
		t.Fatal("duplicate alias resolved")
	}
	if created, err := p.RegisterDevice("alice", first, "renamed", fp); err != nil || created {
		t.Fatalf("rename: %v %v", created, err)
	}
	if id, err := p.ResolveDeviceTargetStrict("alice", "renamed"); err != nil || id != first {
		t.Fatalf("rename ID: %s %v", id, err)
	}
	if _, ok := p.ACLGrant("bob", "alice", first); !ok {
		t.Fatal("rename lost ACL")
	}
	rows, err = p.ListDevices("bob")
	if err != nil || len(rows) != 1 || rows[0]["device_id"] != first || rows[0]["display_name"] != "renamed" {
		t.Fatalf("shared device: %+v %v", rows, err)
	}
	// The real resolver carries the old trust pin to the new canonical ID.
	r := New(EnvTokenStore("owner-token:alice"))
	r.SetAdmin(p)
	srv := httptest.NewServer(r.Handler())
	defer srv.Close()
	known := transport.NewMemStore()
	if err := known.Pin("alice/fcs-ubuntu", fp, false); err != nil {
		t.Fatal(err)
	}
	c := client.NewWith(nil, known, srv.URL, "owner-token", "http")
	if target, err := c.PinServer(context.Background(), "alice/"+first, fp, false); err != nil || target != "alice/"+first {
		t.Fatalf("pin migration: %s %v", target, err)
	}
	if pin, ok := known.GetByName("alice/" + first); !ok || pin.Fingerprint != fp {
		t.Fatal("old pin was not carried over")
	}
	// Rotating a certificate retains the ID, leaving clients to enforce their pin.
	rotated := transport.Fingerprint([]byte("replacement"))
	if created, err := p.RegisterDevice("alice", first, "renamed", rotated); err != nil || created {
		t.Fatalf("rotation: %v %v", created, err)
	}
	rows, _ = p.ListDevices("bob")
	if rows[0]["fingerprint"] != rotated || rows[0]["legacy_name"] != "fcs-ubuntu" {
		t.Fatalf("rotation/migration metadata: %+v", rows)
	}
	if _, err := c.PinServer(context.Background(), "alice/"+first, rotated, false); err == nil {
		t.Fatal("certificate rotation silently replaced pin")
	} else {
		var mismatch *transport.MismatchError
		if !errors.As(err, &mismatch) {
			t.Fatalf("rotation error: %v", err)
		}
	}
	if err := r.allowLegacyRegistration("alice", "fcs-ubuntu"); err == nil {
		t.Fatal("old hostname was resurrected after migration")
	}
	// Legacy upserts cannot replace an upgraded identity.
	p.UpsertDevice("alice", first, fp)
	rows, _ = p.ListDevices("bob")
	if rows[0]["fingerprint"] != rotated {
		t.Fatal("legacy registration overwrote UUID record")
	}
}

// pgDeviceIDStore returns a fully migrated store plus a fatal-on-error exec helper.
func pgDeviceIDStore(t *testing.T) (*PGStore, *sql.DB, func(string, ...any)) {
	t.Helper()
	db := deviceIDTestDB(t)
	if err := runMigrations(db, migrationFiles); err != nil {
		t.Fatal(err)
	}
	return &PGStore{db: db}, db, func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
}

// The production failure this fixes: a Mac agent restarted after an upgrade,
// os.Hostname() had drifted from "zyldeMacBook-Pro.local" to "localhost", and
// the relay refused to promote the legacy row because the *name* differed. The
// installation registered as a second device and the original went offline
// forever, taking its alias, grants and audit history with it.
func TestDeviceIDPostgresPromotesLegacyRowWhenOnlyTheNameChanged(t *testing.T) {
	p, db, exec := pgDeviceIDStore(t)
	fp := transport.Fingerprint([]byte("mac certificate"))
	exec(`INSERT INTO devices(owner_namespace,device_id,display_name,fingerprint,alias,uses_device_id)
	       VALUES ('alice','zyldeMacBook-Pro.local','zyldeMacBook-Pro.local',$1,'我的 Mac',false)`, fp)
	exec(`INSERT INTO acl(owner_namespace,device,grantee_namespace,perms) VALUES ('alice','zyldeMacBook-Pro.local','bob','read')`)
	exec(`INSERT INTO audit(namespace,device,event) VALUES ('alice','zyldeMacBook-Pro.local','dial')`)

	id := uuid.NewString()
	if created, err := p.RegisterDevice("alice", id, "localhost", fp); err != nil || created {
		t.Fatalf("promotion by fingerprint: created=%v err=%v", created, err)
	}
	rows, err := p.ListDevices("alice")
	if err != nil || len(rows) != 1 {
		t.Fatalf("the installation must stay a single device: %+v %v", rows, err)
	}
	row := rows[0]
	// The label follows the agent's current --name, as it does on any rename;
	// the previous routing key survives as legacy_name so controllers can carry
	// their pin over and audit rows keep their meaning.
	if row["device_id"] != id || row["display_name"] != "localhost" || row["legacy_name"] != "zyldeMacBook-Pro.local" {
		t.Fatalf("promoted row: %+v", row)
	}
	if row["alias"] != "我的 Mac" {
		t.Fatalf("alias lost in promotion: %+v", row)
	}
	if _, ok := p.ACLGrant("bob", "alice", id); !ok {
		t.Fatal("grant did not follow the promotion")
	}
	var audits int
	if err := db.QueryRow(`SELECT count(*) FROM audit WHERE namespace='alice' AND device=$1`, id).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("audit association: %d %v", audits, err)
	}
	r := New(EnvTokenStore("owner-token:alice"))
	r.SetAdmin(p)
	if err := r.allowLegacyRegistration("alice", "zyldeMacBook-Pro.local"); err == nil {
		t.Fatal("an outdated agent resurrected the promoted hostname")
	}
	// A later restart under yet another hostname is an ordinary rename, not a
	// second promotion.
	if created, err := p.RegisterDevice("alice", id, "bogon", fp); err != nil || created {
		t.Fatalf("rename after promotion: created=%v err=%v", created, err)
	}
	if rows, err := p.ListDevices("alice"); err != nil || len(rows) != 1 || rows[0]["display_name"] != "bogon" {
		t.Fatalf("rename after promotion: %+v %v", rows, err)
	}
}

// The name is only a label, so it must not be able to hand one installation's
// grants to another. Only the certificate fingerprint promotes a legacy row.
func TestDeviceIDPostgresRefusesPromotionOnFingerprintMismatch(t *testing.T) {
	p, _, exec := pgDeviceIDStore(t)
	fp := transport.Fingerprint([]byte("original certificate"))
	other := transport.Fingerprint([]byte("someone else's certificate"))
	exec(`INSERT INTO devices(owner_namespace,device_id,display_name,fingerprint,uses_device_id)
	       VALUES ('alice','shared-name','shared-name',$1,false)`, fp)
	exec(`INSERT INTO acl(owner_namespace,device,grantee_namespace,perms) VALUES ('alice','shared-name','bob','read')`)

	id := uuid.NewString()
	if created, err := p.RegisterDevice("alice", id, "shared-name", other); err != nil || !created {
		t.Fatalf("a different installation must register separately: created=%v err=%v", created, err)
	}
	if _, ok := p.ACLGrant("bob", "alice", id); ok {
		t.Fatal("a same-name device with a different certificate inherited the grant")
	}
	rows, err := p.ListDevices("alice")
	if err != nil || len(rows) != 2 {
		t.Fatalf("two separate devices: %+v %v", rows, err)
	}
	var legacyIntact bool
	if err := p.db.QueryRow(`SELECT NOT uses_device_id FROM devices WHERE owner_namespace='alice' AND device_id='shared-name'`).Scan(&legacyIntact); err != nil || !legacyIntact {
		t.Fatalf("the legacy row was consumed: %v %v", legacyIntact, err)
	}
	// The original agent, still on its old name, keeps working.
	original := New(EnvTokenStore("owner-token:alice"))
	original.SetAdmin(p)
	if err := original.allowLegacyRegistration("alice", "shared-name"); err != nil {
		t.Fatalf("legacy registration broke: %v", err)
	}
}

// Two legacy rows cannot legitimately share a certificate, but a database that
// somehow holds both must promote deterministically rather than by row order.
func TestDeviceIDPostgresPromotesDeterministicallyOnDuplicateFingerprints(t *testing.T) {
	p, _, exec := pgDeviceIDStore(t)
	fp := transport.Fingerprint([]byte("cloned certificate"))
	exec(`INSERT INTO devices(owner_namespace,device_id,display_name,fingerprint,uses_device_id) VALUES ('alice','zulu','zulu',$1,false)`, fp)
	exec(`INSERT INTO devices(owner_namespace,device_id,display_name,fingerprint,uses_device_id) VALUES ('alice','alpha','alpha',$1,false)`, fp)

	id := uuid.NewString()
	// The reported name wins the tie; otherwise the lowest device_id does.
	if created, err := p.RegisterDevice("alice", id, "zulu", fp); err != nil || created {
		t.Fatalf("promotion: created=%v err=%v", created, err)
	}
	rows, err := p.ListDevices("alice")
	if err != nil || len(rows) != 2 {
		t.Fatalf("the other legacy row must survive: %+v %v", rows, err)
	}
	for _, row := range rows {
		if row["device_id"] == id && row["legacy_name"] != "zulu" {
			t.Fatalf("tie broken against the reported name: %+v", row)
		}
	}
}
