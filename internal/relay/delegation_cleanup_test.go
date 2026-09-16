package relay

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"wanctl/internal/delegation"
)

func TestDelegationPostgresCleanupPreservesActiveGrantsAndOwnerData(t *testing.T) {
	p, db, exec := pgDeviceIDStore(t)
	ctx := context.Background()
	ownerToken, err := p.IssueToken("alice", "owner credential", 30)
	if err != nil {
		t.Fatal(err)
	}
	var removed, retained []string
	var removedTokenIDs []int
	var removedJobIDs []string
	for _, status := range []string{"expired", "revoked", "rotated", "rejected", "pending-expired", "active", "pending-active", "recent-expired"} {
		in, approval, _ := delegationFixture(t, p)
		if _, err := p.CreateDelegation(ctx, in); err != nil {
			t.Fatal(err)
		}
		isApproved := status != "rejected" && status != "pending-expired" && status != "pending-active"
		var grant delegation.Request
		var job delegation.Job
		if isApproved {
			grant, err = p.ApproveDelegation(ctx, approval)
			if err != nil {
				t.Fatal(err)
			}
			payload := json.RawMessage(`{"content":"private job data"}`)
			job, _, err = p.BeginJob(ctx, in.ID, "retention", HashToken(string(payload)), payload)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.FinishJob(ctx, in.ID, job.ID, "done", json.RawMessage(`{"content":"private result"}`)); err != nil {
				t.Fatal(err)
			}
		}
		if status != "recent-expired" {
			exec(`UPDATE delegation_requests SET created_at=now()-interval '2 days' WHERE id=$1`, in.ID)
		}
		switch status {
		case "expired", "recent-expired":
			exec(`UPDATE tokens SET expires_at=now()-interval '1 second' WHERE id=$1`, grant.TokenID)
		case "revoked":
			if err := p.RevokeToken("alice", grant.TokenID); err != nil {
				t.Fatal(err)
			}
		case "rotated":
			exec(`UPDATE devices SET fingerprint='changed' WHERE owner_namespace='alice' AND device_id=$1`, approval.Devices[0])
		case "rejected":
			if err := p.RejectDelegation(ctx, in.ID, "alice"); err != nil {
				t.Fatal(err)
			}
		case "pending-expired":
			exec(`UPDATE delegation_requests SET request_expires_at=now()-interval '1 second' WHERE id=$1`, in.ID)
		}
		if status == "active" || status == "pending-active" || status == "recent-expired" {
			retained = append(retained, in.ID)
		} else {
			removed = append(removed, in.ID)
			if isApproved {
				removedTokenIDs = append(removedTokenIDs, grant.TokenID)
				removedJobIDs = append(removedJobIDs, job.ID)
			}
		}
	}
	var beforeAudit int
	if err := db.QueryRow(`SELECT count(*) FROM audit`).Scan(&beforeAudit); err != nil {
		t.Fatal(err)
	}
	if err := p.CleanupDelegations(ctx, time.Hour); !errors.Is(err, delegation.ErrInvalid) {
		t.Fatalf("unsafe retention accepted: %v", err)
	}
	if err := p.CleanupDelegations(ctx, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, id := range removed {
		if _, err := p.GetDelegation(ctx, id); !errors.Is(err, delegation.ErrNotFound) {
			t.Fatalf("inactive grant %s survived: %v", id, err)
		}
	}
	for _, id := range retained {
		if _, err := p.GetDelegation(ctx, id); err != nil {
			t.Fatalf("active/recent grant %s removed: %v", id, err)
		}
	}
	for _, tableAndQuery := range []string{
		`SELECT count(*) FROM delegation_devices WHERE grant_id=ANY($1::text[])`,
		`SELECT count(*) FROM delegation_jobs WHERE grant_id=ANY($1::text[])`,
	} {
		var count int
		if err := db.QueryRow(tableAndQuery, removed).Scan(&count); err != nil || count != 0 {
			t.Fatalf("inactive data survived: count=%d err=%v", count, err)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM tokens WHERE id=ANY($1::integer[])`, removedTokenIDs).Scan(&count); err != nil || count != 0 {
		t.Fatalf("inactive credentials survived: count=%d err=%v", count, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM delegation_jobs WHERE id=ANY($1::text[])`, removedJobIDs).Scan(&count); err != nil || count != 0 {
		t.Fatalf("private job results survived: count=%d err=%v", count, err)
	}
	if ns, ok := p.Resolve(ownerToken); !ok || ns != "alice" {
		t.Fatal("cleanup removed owner credential")
	}
	if err := db.QueryRow(`SELECT count(*) FROM audit`).Scan(&count); err != nil || count != beforeAudit {
		t.Fatalf("audit changed: count=%d want=%d err=%v", count, beforeAudit, err)
	}
	if err := p.CleanupDelegations(ctx, 24*time.Hour); err != nil {
		t.Fatalf("repeat cleanup: %v", err)
	}
}

func TestDelegationPostgresCleanupBatchesAcrossConcurrentWorkers(t *testing.T) {
	p, db, exec := pgDeviceIDStore(t)
	exec(`INSERT INTO delegation_requests(id,ticket_hash,token_hash,label,controller_fingerprint,created_at,request_expires_at)
 SELECT 'old-'||n,'ticket-'||n,'token-'||n,'expired','fp',now()-interval '2 days',now()-interval '1 day'
 FROM generate_series(1,600) n`)
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- p.CleanupDelegations(context.Background(), 24*time.Hour) }()
	}
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM delegation_requests`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("cleanup stopped before clearing the backlog: count=%d err=%v", count, err)
	}
}
