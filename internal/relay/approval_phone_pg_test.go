package relay

import (
	"errors"
	"strings"
	"testing"
)

// Opt in with WANCTL_TEST_POSTGRES. Only an owned device can be the approval
// phone, a namespace has at most one, and removing the device removes the
// designation with it (ADR 0015).
func TestApprovalPhoneDesignation(t *testing.T) {
	db := deviceIDTestDB(t)
	if err := runMigrations(db, migrationFiles); err != nil {
		t.Fatal(err)
	}
	fp := "sha256:" + strings.Repeat("ab", 32)
	if _, err := db.Exec(`INSERT INTO devices(owner_namespace,device_id,display_name,fingerprint,uses_device_id) VALUES
 ('alice','phone-1','pgbm10',$1,true), ('alice','mac-1','mac',$1,true), ('bob','bob-phone','redmi',$1,true)`, fp); err != nil {
		t.Fatal(err)
	}
	p := &PGStore{db: db}

	if got, err := p.ApprovalPhone("alice"); err != nil || got.Device != "" {
		t.Fatalf("unset phone = %+v, %v", got, err)
	}
	if _, err := p.SetApprovalPhone("alice", "bob-phone"); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("designating someone else's device: err = %v, want ErrDeviceNotFound", err)
	}
	if got, err := p.SetApprovalPhone("alice", "phone-1"); err != nil || got.Device != "phone-1" {
		t.Fatalf("set = %+v, %v", got, err)
	}
	if got, err := p.SetApprovalPhone("alice", "mac-1"); err != nil || got.Device != "mac-1" {
		t.Fatalf("replace = %+v, %v", got, err)
	}
	all, err := p.ListApprovalPhones()
	if err != nil || len(all) != 1 || all[0].Namespace != "alice" || all[0].Device != "mac-1" {
		t.Fatalf("list = %+v, %v", all, err)
	}
	if err := p.RemoveDevice("alice", "mac-1"); err != nil {
		t.Fatal(err)
	}
	if got, err := p.ApprovalPhone("alice"); err != nil || got.Device != "" {
		t.Fatalf("phone after its device was removed = %+v, %v", got, err)
	}
	if _, err := p.SetApprovalPhone("alice", "phone-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.SetApprovalPhone("alice", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.ApprovalPhone("alice"); got.Device != "" {
		t.Fatalf("cleared phone = %+v", got)
	}
}
