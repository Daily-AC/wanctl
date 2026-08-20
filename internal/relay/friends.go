package relay

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"wanctl/internal/sessionauth"
)

var (
	ErrNoSuchUser   = errors.New("no-such-user")
	ErrNoSuchFriend = errors.New("no-such-friend")
	ErrNotFriends   = errors.New("not-friends")
)

type Friend struct {
	Namespace string    `json:"namespace"`
	Status    string    `json:"status"`
	Direction string    `json:"direction,omitempty"`
	Since     time.Time `json:"since"`
}

type ReceivedShare struct {
	Device string `json:"device"`
	Owner  string `json:"owner"`
	Perms  string `json:"perms"`
}

func (p *PGStore) LookupUser(namespace string) (bool, error) {
	var exists bool
	err := p.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM users WHERE namespace = $1)`, namespace).Scan(&exists)
	return exists, err
}

func (p *PGStore) IsFriend(a, b string) (bool, error) {
	if a == b {
		return false, nil
	}
	var exists bool
	err := p.db.QueryRow(
		`SELECT EXISTS (
		   SELECT 1 FROM friends
		    WHERE LEAST(requester_ns, addressee_ns) = LEAST($1::text, $2::text)
		      AND GREATEST(requester_ns, addressee_ns) = GREATEST($1::text, $2::text)
		      AND status = 'accepted'
		)`, a, b,
	).Scan(&exists)
	return exists, err
}

func validateFriendRequest(requester, addressee, reservedNS string) error {
	if err := guardNamespace(strings.TrimSpace(requester), reservedNS); err != nil {
		return err
	}
	if err := guardNamespace(strings.TrimSpace(addressee), reservedNS); err != nil {
		return err
	}
	if requester == addressee {
		return fmt.Errorf("%w: cannot add yourself", ErrNamespaceConflict)
	}
	return nil
}

func (p *PGStore) FriendRequest(requester, addressee, reservedNS string) (status string, err error) {
	requester = strings.TrimSpace(requester)
	addressee = strings.TrimSpace(addressee)
	if err := validateFriendRequest(requester, addressee, reservedNS); err != nil {
		return "", err
	}
	tx, err := p.db.Begin()
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.Exec(
		`SELECT pg_advisory_xact_lock(hashtext(LEAST($1::text,$2::text) || '|' || GREATEST($1::text,$2::text)))`,
		requester, addressee,
	); err != nil {
		return "", err
	}
	var exists bool
	if err = tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM users WHERE namespace = $1)`, addressee).Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		return "", ErrNoSuchUser
	}
	var existingRequester, existingAddressee, existingStatus string
	err = tx.QueryRow(
		`SELECT requester_ns, addressee_ns, status FROM friends
		  WHERE LEAST(requester_ns, addressee_ns) = LEAST($1::text, $2::text)
		    AND GREATEST(requester_ns, addressee_ns) = GREATEST($1::text, $2::text)
		  FOR UPDATE`, requester, addressee,
	).Scan(&existingRequester, &existingAddressee, &existingStatus)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		err = tx.QueryRow(
			`INSERT INTO friends (requester_ns, addressee_ns) VALUES ($1,$2) RETURNING status`,
			requester, addressee,
		).Scan(&status)
	case err != nil:
		return "", err
	case existingStatus == "accepted" || existingRequester == requester:
		status = existingStatus
	case existingStatus == "pending" && existingAddressee == requester:
		_, err = tx.Exec(
			`UPDATE friends SET status = 'accepted', accepted_at = now()
			  WHERE requester_ns = $1 AND addressee_ns = $2 AND status = 'pending'`,
			existingRequester, existingAddressee,
		)
		status = "accepted"
	default:
		err = fmt.Errorf("invalid friend status %q", existingStatus)
	}
	if err != nil {
		return "", err
	}
	err = tx.Commit()
	return status, err
}

func (p *PGStore) FriendAccept(namespace, requester string) error {
	result, err := p.db.Exec(
		`UPDATE friends SET status = 'accepted', accepted_at = now()
		  WHERE addressee_ns = $1 AND requester_ns = $2 AND status = 'pending'`,
		namespace, requester,
	)
	return friendResult(result, err)
}

func (p *PGStore) FriendDecline(namespace, requester string) error {
	result, err := p.db.Exec(
		`DELETE FROM friends WHERE addressee_ns = $1 AND requester_ns = $2 AND status = 'pending'`,
		namespace, requester,
	)
	return friendResult(result, err)
}

func friendResult(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNoSuchFriend
	}
	return nil
}

func (p *PGStore) FriendRemove(namespace, other string) (err error) {
	tx, err := p.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	result, err := tx.Exec(
		`DELETE FROM friends
		  WHERE LEAST(requester_ns, addressee_ns) = LEAST($1::text, $2::text)
		    AND GREATEST(requester_ns, addressee_ns) = GREATEST($1::text, $2::text)`,
		namespace, other,
	)
	if err = friendResult(result, err); err != nil {
		return err
	}
	if _, err = tx.Exec(
		`UPDATE acl SET revoked_at = now()
		  WHERE revoked_at IS NULL
		    AND ((owner_namespace = $1 AND grantee_namespace = $2)
		      OR (owner_namespace = $2 AND grantee_namespace = $1))`,
		namespace, other,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (p *PGStore) ListFriends(namespace string) ([]Friend, error) {
	rows, err := p.db.Query(
		`SELECT CASE WHEN requester_ns = $1 THEN addressee_ns ELSE requester_ns END,
		        status,
		        CASE WHEN status = 'accepted' THEN ''
		             WHEN requester_ns = $1 THEN 'outgoing' ELSE 'incoming' END,
		        COALESCE(accepted_at, created_at)
		   FROM friends
		  WHERE requester_ns = $1 OR addressee_ns = $1
		  ORDER BY status, COALESCE(accepted_at, created_at), id`, namespace,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	friends := []Friend{}
	for rows.Next() {
		var friend Friend
		if err := rows.Scan(&friend.Namespace, &friend.Status, &friend.Direction, &friend.Since); err != nil {
			return nil, err
		}
		friends = append(friends, friend)
	}
	return friends, rows.Err()
}

func (p *PGStore) ListReceivedACL(namespace string) ([]ReceivedShare, error) {
	rows, err := p.db.Query(
		`SELECT device, owner_namespace, perms FROM acl
		  WHERE grantee_namespace = $1 AND revoked_at IS NULL ORDER BY id DESC`, namespace,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	shares := []ReceivedShare{}
	for rows.Next() {
		var share ReceivedShare
		if err := rows.Scan(&share.Device, &share.Owner, &share.Perms); err != nil {
			return nil, err
		}
		shares = append(shares, share)
	}
	return shares, rows.Err()
}

func (p *PGStore) GrantACL(namespace, device, grantee, perms string) (id int, err error) {
	if namespace == grantee {
		return 0, ErrNotFriends
	}
	if perms == "" {
		perms = "exec,read,write"
	}
	caps, err := sessionauth.ParseGrant(perms)
	if err != nil {
		return 0, fmt.Errorf("invalid ACL permissions: %w", err)
	}
	tx, err := p.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	var friendID int
	err = tx.QueryRow(
		`SELECT id FROM friends
		  WHERE LEAST(requester_ns, addressee_ns) = LEAST($1::text, $2::text)
		    AND GREATEST(requester_ns, addressee_ns) = GREATEST($1::text, $2::text)
		    AND status = 'accepted'
		  FOR SHARE`, namespace, grantee,
	).Scan(&friendID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFriends
	}
	if err != nil {
		return 0, err
	}
	err = tx.QueryRow(
		`INSERT INTO acl (owner_namespace, device, grantee_namespace, perms)
		 VALUES ($1,$2,$3,$4) RETURNING id`,
		namespace, device, grantee, caps.String(),
	).Scan(&id)
	if err != nil {
		return 0, err
	}
	err = tx.Commit()
	return id, err
}

func (p *PGStore) RevokeACLMatch(namespace string, id int, device, grantee string) (bool, error) {
	var result sql.Result
	var err error
	if id != 0 {
		result, err = p.db.Exec(
			`UPDATE acl SET revoked_at = now()
			  WHERE id = $1 AND owner_namespace = $2 AND revoked_at IS NULL`, id, namespace,
		)
	} else {
		result, err = p.db.Exec(
			`UPDATE acl SET revoked_at = now()
			  WHERE owner_namespace = $1 AND device = $2 AND grantee_namespace = $3 AND revoked_at IS NULL`,
			namespace, device, grantee,
		)
	}
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}
