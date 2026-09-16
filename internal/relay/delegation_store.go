package relay

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"wanctl/internal/delegation"
	"wanctl/internal/transport"
)

const (
	maxPendingDelegations = 1000
	maxDelegationDevices  = 16
	maxDelegationJobs     = 64
	maxDelegationPayload  = 32 << 10
	maxDelegationResult   = 256 << 10
)

var _ delegation.Store = (*PGStore)(nil)
var _ delegation.JobStore = (*PGStore)(nil)

func validDelegationID(s string) bool {
	if len(s) < 1 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func validDelegationHash(s string) bool {
	_, err := hex.DecodeString(s)
	return len(s) == 64 && err == nil && s == strings.ToLower(s)
}

type delegationQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// readDelegation derives live status from the existing token and current device
// identities. Revocation and certificate rotation take effect without a second ACL.
func readDelegation(ctx context.Context, q delegationQuerier, id, ticketHash string) (delegation.Request, error) {
	var out delegation.Request
	var expires sql.NullTime
	err := q.QueryRowContext(ctx, `SELECT r.id,r.label,r.controller_fingerprint,
 CASE WHEN r.status='approved' AND (t.id IS NULL OR t.revoked_at IS NOT NULL OR
   EXISTS (SELECT 1 FROM delegation_devices g LEFT JOIN devices d
     ON d.owner_namespace=g.namespace AND d.device_id=g.device_id AND d.uses_device_id
     AND d.fingerprint=g.fingerprint WHERE g.grant_id=r.id AND d.id IS NULL)) THEN 'revoked'
 WHEN r.status='approved' AND (t.expires_at IS NULL OR t.expires_at<=now()) THEN 'expired'
 WHEN r.status='pending' AND r.request_expires_at<=now() THEN 'expired'
 ELSE r.status END,
 r.namespace,r.created_at,r.request_expires_at,t.expires_at,COALESCE(r.token_id,0)
 FROM delegation_requests r LEFT JOIN tokens t ON t.id=r.token_id
 WHERE r.id=$1 AND ($2='' OR r.ticket_hash=$2)`, id, ticketHash).Scan(
		&out.ID, &out.Label, &out.ControllerFingerprint, &out.Status, &out.Namespace,
		&out.CreatedAt, &out.RequestExpiresAt, &expires, &out.TokenID)
	if errors.Is(err, sql.ErrNoRows) {
		return out, delegation.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if expires.Valid {
		out.ExpiresAt = &expires.Time
	}
	rows, err := q.QueryContext(ctx, `SELECT namespace,device_id,fingerprint FROM delegation_devices WHERE grant_id=$1 ORDER BY device_id`, id)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	out.Devices = []delegation.Device{}
	for rows.Next() {
		var d delegation.Device
		if err := rows.Scan(&d.Namespace, &d.ID, &d.Fingerprint); err != nil {
			return out, err
		}
		out.Devices = append(out.Devices, d)
	}
	return out, rows.Err()
}

func (p *PGStore) CreateDelegation(ctx context.Context, in delegation.NewRequest) (delegation.Request, error) {
	if !validDelegationID(in.ID) || !validDelegationHash(in.TicketHash) || !validDelegationHash(in.TokenHash) ||
		len(in.Label) == 0 || len(in.Label) > 200 || !transport.ValidFingerprint(in.ControllerFingerprint) {
		return delegation.Request{}, delegation.ErrInvalid
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return delegation.Request{}, err
	}
	defer tx.Rollback()
	// Serialize both the pending quota and idempotent creates across instances.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('wanctl_delegation_requests'))`); err != nil {
		return delegation.Request{}, err
	}
	var ticketHash, tokenHash, fp string
	err = tx.QueryRowContext(ctx, `SELECT ticket_hash,token_hash,controller_fingerprint FROM delegation_requests WHERE id=$1`, in.ID).Scan(&ticketHash, &tokenHash, &fp)
	if err == nil {
		if ticketHash != in.TicketHash || tokenHash != in.TokenHash || fp != in.ControllerFingerprint {
			return delegation.Request{}, delegation.ErrConflict
		}
		return readDelegation(ctx, tx, in.ID, in.TicketHash)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return delegation.Request{}, err
	}
	now := time.Now()
	if !in.RequestExpiresAt.After(now) {
		return delegation.Request{}, delegation.ErrInvalid
	}
	if in.RequestExpiresAt.After(now.Add(10 * time.Minute)) {
		in.RequestExpiresAt = now.Add(10 * time.Minute)
	}
	var pending int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM delegation_requests WHERE status='pending' AND request_expires_at>now()`).Scan(&pending); err != nil {
		return delegation.Request{}, err
	}
	if pending >= maxPendingDelegations {
		return delegation.Request{}, delegation.ErrLimit
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO delegation_requests(id,ticket_hash,token_hash,label,controller_fingerprint,request_expires_at)
 VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, in.ID, in.TicketHash, in.TokenHash, in.Label, in.ControllerFingerprint, in.RequestExpiresAt)
	if err != nil {
		return delegation.Request{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return delegation.Request{}, delegation.ErrConflict
	}
	out, err := readDelegation(ctx, tx, in.ID, in.TicketHash)
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

func (p *PGStore) GetDelegation(ctx context.Context, id string) (delegation.Request, error) {
	return readDelegation(ctx, p.db, id, "")
}

func (p *PGStore) GetDelegationByTicket(ctx context.Context, id, ticketHash string) (delegation.Request, error) {
	if !validDelegationHash(ticketHash) {
		return delegation.Request{}, delegation.ErrNotFound
	}
	return readDelegation(ctx, p.db, id, ticketHash)
}

func (p *PGStore) ApproveDelegation(ctx context.Context, in delegation.Approval) (delegation.Request, error) {
	if guardNamespace(in.Namespace, "") != nil || len(in.Namespace) > 128 || in.Minutes < 1 || in.Minutes > 60 ||
		len(in.Devices) < 1 || len(in.Devices) > maxDelegationDevices || !transport.ValidFingerprint(in.ControllerFingerprint) ||
		len(in.DeviceFingerprints) != len(in.Devices) {
		return delegation.Request{}, delegation.ErrInvalid
	}
	ids := append([]string(nil), in.Devices...)
	sort.Strings(ids)
	for i, id := range ids {
		if !transport.ValidDeviceID(id) || !transport.ValidFingerprint(in.DeviceFingerprints[id]) || (i > 0 && id == ids[i-1]) {
			return delegation.Request{}, delegation.ErrInvalid
		}
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return delegation.Request{}, err
	}
	defer tx.Rollback()
	var status, owner, fp, tokenHash string
	var expiry time.Time
	err = tx.QueryRowContext(ctx, `SELECT status,namespace,controller_fingerprint,token_hash,request_expires_at
 FROM delegation_requests WHERE id=$1 FOR UPDATE`, in.RequestID).Scan(&status, &owner, &fp, &tokenHash, &expiry)
	if errors.Is(err, sql.ErrNoRows) {
		return delegation.Request{}, delegation.ErrNotFound
	}
	if err != nil {
		return delegation.Request{}, err
	}
	if owner != "" && owner != in.Namespace {
		return delegation.Request{}, delegation.ErrForbidden
	}
	if status != "pending" {
		return delegation.Request{}, delegation.ErrConflict
	}
	if !time.Now().Before(expiry) {
		return delegation.Request{}, delegation.ErrExpired
	}
	if fp != in.ControllerFingerprint {
		return delegation.Request{}, delegation.ErrConflict
	}
	for _, id := range ids {
		var actual string
		err = tx.QueryRowContext(ctx, `SELECT fingerprint FROM devices WHERE owner_namespace=$1 AND device_id=$2 AND uses_device_id FOR SHARE`, in.Namespace, id).Scan(&actual)
		if errors.Is(err, sql.ErrNoRows) {
			return delegation.Request{}, delegation.ErrForbidden
		}
		if err != nil {
			return delegation.Request{}, err
		}
		if actual != in.DeviceFingerprints[id] {
			return delegation.Request{}, delegation.ErrConflict
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO delegation_devices(grant_id,namespace,device_id,fingerprint) VALUES ($1,$2,$3,$4)`, in.RequestID, in.Namespace, id, actual); err != nil {
			return delegation.Request{}, err
		}
	}
	var tokenID int
	err = tx.QueryRowContext(ctx, `INSERT INTO tokens(namespace,kind,hash,label,expires_at)
 SELECT $2,'delegated',token_hash,label,now()+($3 * interval '1 minute') FROM delegation_requests WHERE id=$1 RETURNING id`, in.RequestID, in.Namespace, in.Minutes).Scan(&tokenID)
	if err != nil {
		return delegation.Request{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE delegation_requests SET status='approved',namespace=$2,token_id=$3,decided_at=now() WHERE id=$1`, in.RequestID, in.Namespace, tokenID); err != nil {
		return delegation.Request{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit(namespace,token_id,event) VALUES ($1,$2,$3)`, in.Namespace, tokenID, "delegation-approved:"+in.RequestID); err != nil {
		return delegation.Request{}, err
	}
	out, err := readDelegation(ctx, tx, in.RequestID, "")
	if err != nil {
		return out, err
	}
	return out, tx.Commit()
}

func (p *PGStore) RejectDelegation(ctx context.Context, id, ns string) error {
	if guardNamespace(ns, "") != nil {
		return delegation.ErrInvalid
	}
	result, err := p.db.ExecContext(ctx, `UPDATE delegation_requests SET status='rejected',namespace=$2,decided_at=now()
 WHERE id=$1 AND status='pending' AND request_expires_at>now() AND namespace=''`, id, ns)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 1 {
		return nil
	}
	out, err := p.GetDelegation(ctx, id)
	if err != nil {
		return err
	}
	if out.Namespace != "" && out.Namespace != ns {
		return delegation.ErrForbidden
	}
	if out.Status == "rejected" {
		return nil
	}
	if out.Status == "expired" {
		return delegation.ErrExpired
	}
	return delegation.ErrConflict
}

// ResolveAccess preserves the scope on delegated tokens. Legacy Resolve must
// reject them because a namespace alone would turn a grant into owner rights.
func (p *PGStore) ResolveAccess(raw string) (delegation.Access, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out delegation.Access
	var id int
	var kind, grantID string
	var expires sql.NullTime
	err := p.db.QueryRowContext(ctx, `SELECT t.id,t.namespace,t.kind,t.expires_at,COALESCE(r.id,'')
 FROM tokens t LEFT JOIN delegation_requests r ON r.token_id=t.id
 WHERE t.hash=$1 AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>now())`, HashToken(raw)).Scan(&id, &out.Namespace, &kind, &expires, &grantID)
	if err != nil {
		return delegation.Access{}, false
	}
	out.CredentialID = strconv.Itoa(id)
	if expires.Valid {
		out.ExpiresAt = expires.Time
	}
	if kind != "delegated" {
		return out, true
	}
	grant, err := p.GetDelegation(ctx, grantID)
	if err != nil || grant.Status != "approved" || grant.Namespace != out.Namespace || len(grant.Devices) == 0 || grant.ExpiresAt == nil {
		return delegation.Access{}, false
	}
	out.Delegated = true
	out.GrantID = grant.ID
	out.ExpiresAt = *grant.ExpiresAt
	out.ControllerFingerprint = grant.ControllerFingerprint
	out.Devices = grant.Devices
	return out, true
}

func scanDelegationJob(row interface{ Scan(...any) error }) (delegation.Job, error) {
	var out delegation.Job
	var payload, result []byte
	err := row.Scan(&out.ID, &out.GrantID, &out.RequestID, &out.PayloadHash, &payload, &out.State, &result, &out.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return out, delegation.ErrNotFound
	}
	out.Payload = json.RawMessage(payload)
	out.Result = json.RawMessage(result)
	return out, err
}

const delegationJobColumns = `id,grant_id,request_id,payload_hash,payload,state,result,created_at`

func (p *PGStore) BeginJob(ctx context.Context, grant, rid, payloadHash string, payload json.RawMessage) (delegation.Job, bool, error) {
	if !validDelegationID(grant) || !validDelegationID(rid) || !validDelegationHash(payloadHash) || len(payload) > maxDelegationPayload || !json.Valid(payload) {
		return delegation.Job{}, false, delegation.ErrInvalid
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return delegation.Job{}, false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "delegation-job:"+grant); err != nil {
		return delegation.Job{}, false, err
	}
	old, err := scanDelegationJob(tx.QueryRowContext(ctx, `SELECT `+delegationJobColumns+` FROM delegation_jobs WHERE grant_id=$1 AND request_id=$2`, grant, rid))
	if err == nil {
		if old.PayloadHash != payloadHash {
			return delegation.Job{}, false, delegation.ErrConflict
		}
		return old, false, nil
	}
	if !errors.Is(err, delegation.ErrNotFound) {
		return delegation.Job{}, false, err
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM delegation_jobs WHERE grant_id=$1`, grant).Scan(&count); err != nil {
		return delegation.Job{}, false, err
	}
	if count >= maxDelegationJobs {
		return delegation.Job{}, false, delegation.ErrLimit
	}
	// The ledger also fails closed on inactive grants. The adapter must still
	// recheck immediately before dispatch to cover revocation after this write.
	access, err := readDelegation(ctx, tx, grant, "")
	if err != nil {
		return delegation.Job{}, false, err
	}
	if access.Status != "approved" {
		return delegation.Job{}, false, delegation.ErrForbidden
	}
	id := "j_" + randHex(16)
	job, err := scanDelegationJob(tx.QueryRowContext(ctx, `INSERT INTO delegation_jobs(id,grant_id,request_id,payload_hash,payload) VALUES ($1,$2,$3,$4,$5) RETURNING `+delegationJobColumns, id, grant, rid, payloadHash, []byte(payload)))
	if err != nil {
		return delegation.Job{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return delegation.Job{}, false, err
	}
	return job, true, nil
}

func (p *PGStore) FinishJob(ctx context.Context, grant, id, state string, result json.RawMessage) error {
	if state != "done" && state != "failed" && state != "unknown" || len(result) > maxDelegationResult || !json.Valid(result) {
		return delegation.ErrInvalid
	}
	res, err := p.db.ExecContext(ctx, `UPDATE delegation_jobs SET state=$3,result=$4,finished_at=now() WHERE grant_id=$1 AND id=$2 AND state='running'`, grant, id, state, []byte(result))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return nil
	}
	_, err = p.GetJob(ctx, grant, id)
	if err != nil {
		return err
	}
	return delegation.ErrConflict
}

func (p *PGStore) GetJob(ctx context.Context, grant, id string) (delegation.Job, error) {
	return scanDelegationJob(p.db.QueryRowContext(ctx, `SELECT `+delegationJobColumns+` FROM delegation_jobs WHERE grant_id=$1 AND id=$2`, grant, id))
}
