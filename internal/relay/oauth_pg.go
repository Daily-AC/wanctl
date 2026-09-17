package relay

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// Postgres backing for the MCP OAuth flow. Kept beside the other per-feature
// store files (notify_pg.go, lark_pg.go) rather than folded into admin.go: the
// rows have their own lifecycle and nothing to do with the admin surface.

func (p *PGStore) PutOAuthClient(c OAuthClient) error {
	uris, err := json.Marshal(c.RedirectURIs)
	if err != nil {
		return err
	}
	_, err = p.db.Exec(
		`INSERT INTO oauth_clients (id, secret_hash, name, redirect_uris, auth_method, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		c.ID, c.SecretHash, c.Name, uris, c.AuthMethod, c.CreatedAt)
	return err
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

func (p *PGStore) RevokeOAuthRefresh(hash string) error {
	_, err := p.db.Exec(
		`UPDATE oauth_refresh_tokens SET revoked_at = now() WHERE hash = $1 AND revoked_at IS NULL`, hash)
	return err
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
