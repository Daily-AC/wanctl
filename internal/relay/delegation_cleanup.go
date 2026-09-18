package relay

import (
	"context"
	"database/sql"
	"time"

	"wanctl/internal/delegation"
)

// CleanupDelegations removes inactive delegation records after the retention
// floor. It may only be enabled with time-bounded browser tickets: the adapter
// must reject an old ticket before looking up or creating its request, so
// deletion cannot revive an old request ID. That is why the floor is
// delegation.MinRetention and not a round day: retention is counted from the
// request row's created_at, a row can be created a RequestWindow after its
// ticket was issued, and the grant approved on it may run a full
// MaxGrantMinutes from there — a day is now exactly the boundary rather than
// safely past it. Owner tokens and audit records are retained.
func (p *PGStore) CleanupDelegations(ctx context.Context, retention time.Duration) error {
	if retention < delegation.MinRetention {
		return delegation.ErrInvalid
	}
	cutoff := time.Now().Add(-retention)
	for {
		deleted, err := p.cleanupDelegationBatch(ctx, cutoff)
		if err != nil || deleted == 0 {
			return err
		}
	}
}

func (p *PGStore) cleanupDelegationBatch(ctx context.Context, cutoff time.Time) (int, error) {
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Lock only requests, not the outer-joined tokens. Approval takes the same
	// request lock; independent cleanup workers skip each other's current batch.
	rows, err := tx.QueryContext(ctx, `SELECT r.id,t.id
 FROM delegation_requests r LEFT JOIN tokens t ON t.id=r.token_id
 WHERE r.created_at<$1 AND (
   r.status='rejected' OR
   (r.status='pending' AND r.request_expires_at<=now()) OR
   (r.status='approved' AND (t.id IS NULL OR t.revoked_at IS NOT NULL OR
     t.expires_at IS NULL OR t.expires_at<=now() OR
     EXISTS (SELECT 1 FROM delegation_devices g LEFT JOIN devices d
       ON d.owner_namespace=g.namespace AND d.device_id=g.device_id
       AND d.uses_device_id AND d.fingerprint=g.fingerprint
       WHERE g.grant_id=r.id AND d.id IS NULL)))
 ) ORDER BY r.created_at,r.id LIMIT 256 FOR UPDATE OF r SKIP LOCKED`, cutoff)
	if err != nil {
		return 0, err
	}
	var ids []string
	var tokenIDs []int64
	for rows.Next() {
		var id string
		var tokenID sql.NullInt64
		if err := rows.Scan(&id, &tokenID); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
		if tokenID.Valid {
			tokenIDs = append(tokenIDs, tokenID.Int64)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	for _, statement := range []string{
		`DELETE FROM delegation_jobs WHERE grant_id=ANY($1::text[])`,
		`DELETE FROM delegation_devices WHERE grant_id=ANY($1::text[])`,
		`DELETE FROM delegation_requests WHERE id=ANY($1::text[])`,
	} {
		if _, err := tx.ExecContext(ctx, statement, ids); err != nil {
			return 0, err
		}
	}
	if len(tokenIDs) > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM tokens WHERE id=ANY($1::integer[]) AND kind='delegated'`, tokenIDs); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}
