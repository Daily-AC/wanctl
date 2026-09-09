package relay

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"os"
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
		db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	if os.Getenv("WANCTL_AUTO_MIGRATE") == "0" {
		log.Printf("relay: automatic database migrations disabled by WANCTL_AUTO_MIGRATE=0")
	} else if err := runMigrations(db, migrationFiles); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate postgres: %w", err)
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

// ACLGrant reports a live cross-namespace grant. Nothing stops the same pair
// from holding two live rows, so the managing one wins: the owner turned
// management on for this grantee and revoking is how they turn it off.
func (p *PGStore) ACLGrant(callerNS, targetNS, device string) (Grant, bool) {
	var manage bool
	err := p.db.QueryRow(
		`SELECT manage FROM acl
		   WHERE owner_namespace = $1 AND device = $2
		     AND grantee_namespace = $3 AND revoked_at IS NULL
		   ORDER BY manage DESC
		   LIMIT 1`,
		targetNS, device, callerNS,
	).Scan(&manage)
	if err != nil {
		return Grant{}, false
	}
	return Grant{Manage: manage}, true
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
