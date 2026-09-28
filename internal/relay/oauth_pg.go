package relay

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Postgres backing for the MCP OAuth flow. Kept beside the other per-feature
// store files (notify_pg.go, lark_pg.go) rather than folded into admin.go: the
// rows have their own lifecycle and nothing to do with the admin surface.

// oauthRegisterLock serializes registrations (pg_advisory_xact_lock), so two
// cannot both see room for the last place under the ceiling. The value is
// arbitrary; it only has to be this statement's own.
const oauthRegisterLock = 0x77636f5f726567 // "wco_reg"

// A client "never completed an authorization" when no refresh token names it:
// the first refresh token is written by the code exchange that ends one.
const oauthUnusedClient = `NOT EXISTS (SELECT 1 FROM oauth_refresh_tokens r WHERE r.client_id = c.id)`

func (p *PGStore) RegisterOAuthClient(c OAuthClient, maxUnused int, staleBefore time.Time, inFlight []string) (bool, error) {
	uris, err := json.Marshal(c.RedirectURIs)
	if err != nil {
		return false, err
	}
	if inFlight == nil {
		inFlight = []string{}
	}
	tx, err := p.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock($1)`, oauthRegisterLock); err != nil {
		return false, err
	}
	if _, err := tx.Exec(
		`DELETE FROM oauth_clients c
		  WHERE c.created_at < $1 AND NOT (c.id = ANY($2)) AND `+oauthUnusedClient,
		staleBefore, inFlight); err != nil {
		return false, err
	}
	var unused int
	if err := tx.QueryRow(`SELECT count(*) FROM oauth_clients c WHERE ` + oauthUnusedClient).Scan(&unused); err != nil {
		return false, err
	}
	if unused >= maxUnused {
		// Nothing stored, but the stale rows deleted above are gone for good.
		return false, tx.Commit()
	}
	if _, err := tx.Exec(
		`INSERT INTO oauth_clients (id, secret_hash, name, redirect_uris, auth_method, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		c.ID, c.SecretHash, c.Name, uris, c.AuthMethod, c.CreatedAt); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func (p *PGStore) OAuthClient(id string) (OAuthClient, bool, error) {
	var c OAuthClient
	var uris []byte
	err := p.db.QueryRow(
		`SELECT id, secret_hash, name, redirect_uris, auth_method, created_at
		   FROM oauth_clients WHERE id = $1`, id,
	).Scan(&c.ID, &c.SecretHash, &c.Name, &uris, &c.AuthMethod, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthClient{}, false, nil
	}
	if err != nil {
		return OAuthClient{}, false, err
	}
	if err := json.Unmarshal(uris, &c.RedirectURIs); err != nil {
		return OAuthClient{}, false, err
	}
	return c, true, nil
}

func (p *PGStore) PutOAuthRefresh(t OAuthRefresh) error {
	_, err := p.db.Exec(
		`INSERT INTO oauth_refresh_tokens (hash, client_id, namespace, grant_envelope, expires_at)
		 VALUES ($1,$2,$3,$4,$5)`,
		t.Hash, t.ClientID, t.Namespace, t.Grant, t.ExpiresAt)
	return err
}

func (p *PGStore) OAuthRefresh(hash string) (OAuthRefresh, bool, error) {
	var t OAuthRefresh
	var revoked sql.NullTime
	err := p.db.QueryRow(
		`SELECT hash, client_id, namespace, grant_envelope, expires_at, revoked_at
		   FROM oauth_refresh_tokens WHERE hash = $1`, hash,
	).Scan(&t.Hash, &t.ClientID, &t.Namespace, &t.Grant, &t.ExpiresAt, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthRefresh{}, false, nil
	}
	if err != nil {
		return OAuthRefresh{}, false, err
	}
	if revoked.Valid {
		t.RevokedAt = revoked.Time
	}
	return t, true, nil
}

func (p *PGStore) RevokeOAuthRefresh(hash string) (bool, error) {
	res, err := p.db.Exec(
		`UPDATE oauth_refresh_tokens SET revoked_at = now() WHERE hash = $1 AND revoked_at IS NULL`, hash)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RevokeRelayTokenHash stops the namespace token an OAuth grant minted. It
// matches on hash because the raw token exists only inside sealed envelopes,
// and on namespace so a grant can never reach past its own account.
func (p *PGStore) RevokeRelayTokenHash(namespace, hash string) error {
	_, err := p.db.Exec(
		`UPDATE tokens SET revoked_at = now()
		   WHERE hash = $1 AND namespace = $2 AND revoked_at IS NULL`, hash, namespace)
	return err
}

// ExtendRelayTokenHash moves the expiry of that same token, matched the same
// way. Only a live token moves: one that was revoked or has lapsed stays dead,
// so a refresh cannot bring back a grant the user already ended.
func (p *PGStore) ExtendRelayTokenHash(namespace, hash string, until time.Time) (bool, error) {
	res, err := p.db.Exec(
		`UPDATE tokens SET expires_at = $3
		   WHERE hash = $1 AND namespace = $2 AND kind = 'access' AND revoked_at IS NULL
		     AND (expires_at IS NULL OR expires_at > now())`, hash, namespace, until)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
