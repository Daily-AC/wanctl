package relay

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"wanctl/internal/delegation"
	"wanctl/internal/transport"

	"github.com/google/uuid"
)

func delegationFixture(t *testing.T, p *PGStore) (delegation.NewRequest, delegation.Approval, string) {
	t.Helper()
	id := uuid.NewString()
	deviceID := uuid.NewString()
	deviceFP := transport.Fingerprint([]byte("device-" + id))
	controllerFP := transport.Fingerprint([]byte("controller-" + id))
	if _, err := p.RegisterDevice("alice", deviceID, "test device", deviceFP); err != nil {
		t.Fatal(err)
	}
	raw := "delegation-test-token-" + id
	in := delegation.NewRequest{ID: id, TicketHash: HashToken("ticket-" + id), TokenHash: HashToken(raw), Label: "Browser test", ControllerFingerprint: controllerFP, RequestExpiresAt: time.Now().Add(10 * time.Minute)}
	approval := delegation.Approval{RequestID: id, Namespace: "alice", Devices: []string{deviceID}, Minutes: 5, ControllerFingerprint: controllerFP, DeviceFingerprints: map[string]string{deviceID: deviceFP}}
	return in, approval, raw
}

func TestDelegationPostgresGrantLifecycle(t *testing.T) {
	p, db, exec := pgDeviceIDStore(t)
	ctx := context.Background()
	in, approval, raw := delegationFixture(t, p)
	created, err := p.CreateDelegation(ctx, in)
	if err != nil || created.Status != "pending" {
		t.Fatalf("create: %+v %v", created, err)
	}
	if _, ok := p.ResolveAccess(raw); ok {
		t.Fatal("pending request resolved")
	}
	if replay, err := p.CreateDelegation(ctx, in); err != nil || replay.ID != created.ID {
		t.Fatalf("replay: %+v %v", replay, err)
	}
	conflict := in
	conflict.TokenHash = HashToken("changed")
	if _, err := p.CreateDelegation(ctx, conflict); !errors.Is(err, delegation.ErrConflict) {
		t.Fatalf("create conflict: %v", err)
	}
	if _, err := p.GetDelegationByTicket(ctx, in.ID, HashToken("wrong")); !errors.Is(err, delegation.ErrNotFound) {
		t.Fatalf("ticket bypass: %v", err)
	}
	bad := approval
	bad.Namespace = "bob"
	if _, err := p.ApproveDelegation(ctx, bad); !errors.Is(err, delegation.ErrForbidden) {
		t.Fatalf("non-owner approval: %v", err)
	}
	bad = approval
	bad.ControllerFingerprint = transport.Fingerprint([]byte("other"))
	if _, err := p.ApproveDelegation(ctx, bad); !errors.Is(err, delegation.ErrConflict) {
		t.Fatalf("controller substitution: %v", err)
	}
	bad = approval
	bad.DeviceFingerprints = map[string]string{approval.Devices[0]: transport.Fingerprint([]byte("stale"))}
	if _, err := p.ApproveDelegation(ctx, bad); !errors.Is(err, delegation.ErrConflict) {
		t.Fatalf("device substitution: %v", err)
	}
	approved, err := p.ApproveDelegation(ctx, approval)
	if err != nil || approved.Status != "approved" || approved.TokenID == 0 {
		t.Fatalf("approve: %+v %v", approved, err)
	}
	if _, ok := p.Resolve(raw); ok {
		t.Fatal("delegated token downgraded to owner namespace")
	}
	access, ok := p.ResolveAccess(raw)
	if !ok || !access.Delegated || access.GrantID != in.ID || len(access.Devices) != 1 || !access.Allows("alice/"+approval.Devices[0]) || access.Allows("alice/"+uuid.NewString()) {
		t.Fatalf("scope: %+v %v", access, ok)
	}
	if _, err := p.ApproveDelegation(ctx, approval); !errors.Is(err, delegation.ErrConflict) {
		t.Fatalf("approval changed by replay: %v", err)
	}
	listed, err := p.ListTokens("alice")
	if err != nil || len(listed) != 1 || listed[0]["kind"] != "delegated" || listed[0]["grant_id"] != in.ID {
		t.Fatalf("list: %+v %v", listed, err)
	}
	var stored string
	if err := db.QueryRow(`SELECT hash FROM tokens WHERE id=$1`, approved.TokenID).Scan(&stored); err != nil || stored != HashToken(raw) || stored == raw {
		t.Fatalf("credential storage: %q %v", stored, err)
	}
	if err := p.RevokeToken("bob", approved.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.ResolveAccess(raw); !ok {
		t.Fatal("other namespace revoked token")
	}
	if err := p.RevokeToken("alice", approved.TokenID); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.ResolveAccess(raw); ok {
		t.Fatal("revoked token resolved")
	}
	out, err := p.GetDelegation(ctx, in.ID)
	if err != nil || out.Status != "revoked" {
		t.Fatalf("revocation status: %+v %v", out, err)
	}

	// Device removal and fingerprint replacement invalidate a grant immediately.
	for _, mutation := range []string{"rotate", "remove", "expire"} {
		in, a, raw := delegationFixture(t, p)
		if _, err := p.CreateDelegation(ctx, in); err != nil {
			t.Fatal(err)
		}
		grant, err := p.ApproveDelegation(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		want := "revoked"
		switch mutation {
		case "rotate":
			exec(`UPDATE devices SET fingerprint=$2 WHERE device_id=$1`, a.Devices[0], transport.Fingerprint([]byte("replacement")))
		case "remove":
			exec(`DELETE FROM devices WHERE device_id=$1`, a.Devices[0])
		case "expire":
			exec(`UPDATE tokens SET expires_at=now()-interval '1 second' WHERE id=$1`, grant.TokenID)
			want = "expired"
		}
		if _, ok := p.ResolveAccess(raw); ok {
			t.Fatalf("%s grant resolved", mutation)
		}
		out, err := p.GetDelegation(ctx, in.ID)
		if err != nil || out.Status != want {
			t.Fatalf("%s status: %+v %v", mutation, out, err)
		}
	}
}

func TestDelegationPostgresPendingLimitsAndRejection(t *testing.T) {
	p, _, exec := pgDeviceIDStore(t)
	ctx := context.Background()
	in, a, _ := delegationFixture(t, p)
	in.RequestExpiresAt = time.Now().Add(24 * time.Hour)
	out, err := p.CreateDelegation(ctx, in)
	if err != nil || out.RequestExpiresAt.After(time.Now().Add(10*time.Minute)) {
		t.Fatalf("request TTL bound: %+v %v", out, err)
	}
	for _, minutes := range []int{0, 61} {
		bad := a
		bad.Minutes = minutes
		if _, err := p.ApproveDelegation(ctx, bad); !errors.Is(err, delegation.ErrInvalid) {
			t.Fatalf("duration %d: %v", minutes, err)
		}
	}
	if err := p.RejectDelegation(ctx, in.ID, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := p.RejectDelegation(ctx, in.ID, "alice"); err != nil {
		t.Fatalf("reject replay: %v", err)
	}
	if err := p.RejectDelegation(ctx, in.ID, "bob"); !errors.Is(err, delegation.ErrForbidden) {
		t.Fatalf("reject scope: %v", err)
	}
	if _, err := p.ApproveDelegation(ctx, a); !errors.Is(err, delegation.ErrConflict) {
		t.Fatalf("rejected approval: %v", err)
	}
	second, a, _ := delegationFixture(t, p)
	if _, err := p.CreateDelegation(ctx, second); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE delegation_requests SET request_expires_at=now()-interval '1 second' WHERE id=$1`, second.ID)
	if _, err := p.ApproveDelegation(ctx, a); !errors.Is(err, delegation.ErrExpired) {
		t.Fatalf("expired approval: %v", err)
	}
	// The queue is globally bounded even under many independent tickets.
	exec(`INSERT INTO delegation_requests(id,ticket_hash,token_hash,label,controller_fingerprint,request_expires_at)
 SELECT 'bulk-'||n,'ticket-'||n,'token-'||n,'pending','fp',now()+interval '10 minutes' FROM generate_series(1,1000) n`)
	third, _, _ := delegationFixture(t, p)
	if _, err := p.CreateDelegation(ctx, third); !errors.Is(err, delegation.ErrLimit) {
		t.Fatalf("pending limit: %v", err)
	}
}

func TestDelegationPostgresJobLedgerConcurrentReplay(t *testing.T) {
	p, _, _ := pgDeviceIDStore(t)
	ctx := context.Background()
	in, a, _ := delegationFixture(t, p)
	if _, err := p.CreateDelegation(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ApproveDelegation(ctx, a); err != nil {
		t.Fatal(err)
	}
	payload := json.RawMessage(`{"device":"test","command":"printf hello"}`)
	hash := HashToken(string(payload))
	var newJobs atomic.Int32
	var wg sync.WaitGroup
	ids := make(chan string, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job, fresh, err := p.BeginJob(ctx, in.ID, "same-rid", hash, payload)
			if err != nil {
				t.Errorf("begin: %v", err)
				return
			}
			if fresh {
				newJobs.Add(1)
			}
			ids <- job.ID
		}()
	}
	wg.Wait()
	close(ids)
	var id string
	for got := range ids {
		if id != "" && got != id {
			t.Fatalf("duplicate IDs: %s %s", id, got)
		}
		id = got
	}
	if newJobs.Load() != 1 {
		t.Fatalf("executions: %d", newJobs.Load())
	}
	if _, _, err := p.BeginJob(ctx, in.ID, "same-rid", HashToken("changed"), payload); !errors.Is(err, delegation.ErrConflict) {
		t.Fatalf("payload conflict: %v", err)
	}
	// A new store instance sees the persisted running job and never dispatches again.
	restarted := &PGStore{db: p.db}
	job, fresh, err := restarted.BeginJob(ctx, in.ID, "same-rid", hash, payload)
	if err != nil || fresh || job.ID != id || job.State != "running" {
		t.Fatalf("restart: %+v %v %v", job, fresh, err)
	}
	if err := p.FinishJob(ctx, "other-grant", id, "done", json.RawMessage(`{"ok":true}`)); !errors.Is(err, delegation.ErrNotFound) {
		t.Fatalf("finish scope: %v", err)
	}
	if err := p.FinishJob(ctx, in.ID, id, "done", json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := p.FinishJob(ctx, in.ID, id, "failed", json.RawMessage(`{"ok":false}`)); !errors.Is(err, delegation.ErrConflict) {
		t.Fatalf("terminal overwrite: %v", err)
	}
	got, err := p.GetJob(ctx, in.ID, id)
	if err != nil || got.State != "done" || !strings.Contains(string(got.Result), "true") {
		t.Fatalf("result: %+v %v", got, err)
	}
	if _, err := p.GetJob(ctx, "other-grant", id); !errors.Is(err, delegation.ErrNotFound) {
		t.Fatalf("get scope: %v", err)
	}
}

func TestDelegationPostgresAdminNamespaceAndInspect(t *testing.T) {
	p, _, _ := pgDeviceIDStore(t)
	ctx := context.Background()
	in, approval, raw := delegationFixture(t, p)
	if _, err := p.CreateDelegation(ctx, in); err != nil {
		t.Fatal(err)
	}
	r := New(p)
	r.SetAdmin(p)
	r.SetAdminSecret("admin-secret")
	r.SetPortalNS("custom-portal")
	handler := r.Handler()
	request := func(method, path, body, secret string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-Admin-Secret", secret)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}
	for _, path := range []string{"/admin/delegations/request?id=" + in.ID + "&namespace=alice", "/admin/delegations/approve", "/admin/delegations/reject", "/admin/tokens/inspect"} {
		if w := request(http.MethodGet, path, "", ""); w.Code != http.StatusForbidden {
			t.Fatalf("unguarded %s: %d", path, w.Code)
		}
	}
	if w := request(http.MethodGet, "/admin/delegations/request?id="+in.ID+"&namespace=alice", "", "admin-secret"); w.Code != 200 || strings.Contains(w.Body.String(), in.TicketHash) || strings.Contains(w.Body.String(), in.TokenHash) {
		t.Fatalf("pending: %d %s", w.Code, w.Body.String())
	}
	bad := approval
	bad.Namespace = "custom-portal"
	body, _ := json.Marshal(bad)
	if w := request(http.MethodPost, "/admin/delegations/approve", string(body), "admin-secret"); w.Code != 403 {
		t.Fatalf("portal grant: %d %s", w.Code, w.Body.String())
	}
	body, _ = json.Marshal(approval)
	if w := request(http.MethodPost, "/admin/delegations/approve", string(body), "admin-secret"); w.Code != 200 {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	if w := request(http.MethodGet, "/admin/delegations/request?id="+in.ID+"&namespace=bob", "", "admin-secret"); w.Code != 403 {
		t.Fatalf("foreign grant: %d %s", w.Code, w.Body.String())
	}
	body, _ = json.Marshal(map[string]string{"token": raw})
	w := request(http.MethodPost, "/admin/tokens/inspect", string(body), "admin-secret")
	var access delegation.Access
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &access) != nil || !access.Delegated || access.GrantID != in.ID || strings.Contains(w.Body.String(), raw) {
		t.Fatalf("inspect: %d %s", w.Code, w.Body.String())
	}
	if w := request(http.MethodPost, "/admin/tokens/resolve", string(body), "admin-secret"); w.Code != 404 {
		t.Fatalf("legacy resolve leak: %d %s", w.Code, w.Body.String())
	}
}
