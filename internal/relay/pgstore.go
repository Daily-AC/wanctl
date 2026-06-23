package relay

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// PGStore is a Postgres-backed TokenStore + ACL checker + audit sink. Tokens are
// stored hashed (SHA-256 hex); the raw token never touches the database.
type PGStore struct {
	db *sql.DB
}

// OpenPG connects to Postgres via a pgx DSN (e.g. the injected DATABASE_URL).
func OpenPG(dsn string) (*PGStore, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &PGStore{db: db}, nil
}

// HashToken returns the canonical stored form of a raw token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Resolve looks up a non-revoked, non-expired token and returns its namespace.
func (p *PGStore) Resolve(token string) (string, bool) {
	var ns string
	err := p.db.QueryRow(
		`SELECT namespace FROM tokens
		   WHERE hash = $1 AND revoked_at IS NULL
		     AND (expires_at IS NULL OR expires_at > now())`,
		HashToken(token),
	).Scan(&ns)
	if err != nil {
		return "", false
	}
	return ns, true
}

// AllowedDial reports whether callerNS may reach targetNS/device: same namespace,
// or a live ACL grant from the target's owner to the caller's namespace.
func (p *PGStore) AllowedDial(callerNS, targetNS, device string) bool {
	if callerNS == targetNS {
		return true
	}
	var one int
	err := p.db.QueryRow(
		`SELECT 1 FROM acl
		   WHERE owner_namespace = $1 AND device = $2
		     AND grantee_namespace = $3 AND revoked_at IS NULL
		   LIMIT 1`,
		targetNS, device, callerNS,
	).Scan(&one)
	return err == nil
}

// Audit records a relay-side metadata event (best-effort; never blocks dials).
func (p *PGStore) Audit(namespace, device, event string) {
	_, _ = p.db.Exec(
		`INSERT INTO audit (namespace, device, event) VALUES ($1,$2,$3)`,
		namespace, device, event,
	)
}

// Close releases the connection pool.
func (p *PGStore) Close() error { return p.db.Close() }
