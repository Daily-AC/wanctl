package relay

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"

	"wanctl/internal/delegation"
)

// Scoped command tokens: see docs/adr/0021-scoped-command-token.md.
const (
	ScopedDefaultDays = 90
	ScopedMaxDays     = 365
	// scopedMaxLive bounds how many live scoped tokens one namespace may hold.
	scopedMaxLive    = 50
	scopedMaxCommand = 4096
	scopedMaxLabel   = 200
)

var (
	ErrScopedInvalid = errors.New("scoped_invalid")
	ErrScopedLimit   = errors.New("scoped_limit")
)

// ScopedToken is what an owner sees about a token they issued. The raw token is
// shown once, at issue, and never again.
type ScopedToken struct {
	ID        int        `json:"id"`
	Label     string     `json:"label"`
	Device    string     `json:"device"`
	Command   string     `json:"command"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

// ScopedTokenStore is optional so static-token relays keep their database-free
// behaviour; PGStore implements it.
type ScopedTokenStore interface {
	IssueScopedToken(namespace, device, command, label string, days int) (string, ScopedToken, error)
	ListScopedTokens(namespace string) ([]ScopedToken, error)
	RevokeScopedToken(namespace string, id int) (bool, error)
}

// validScopedCommand accepts one line of printable text. A scope the owner
// cannot read at a glance in `scoped-token list` is not a scope they can audit,
// so control characters (newlines included) are refused; put a multi-line job in
// a script and scope the call to the script.
func validScopedCommand(command string) bool {
	if command == "" || len(command) > scopedMaxCommand || !utf8.ValidString(command) {
		return false
	}
	for _, r := range command {
		if unicode.IsControl(r) && r != '\t' {
			return false
		}
	}
	return true
}

func (p *PGStore) IssueScopedToken(namespace, device, command, label string, days int) (string, ScopedToken, error) {
	var out ScopedToken
	if days == 0 {
		days = ScopedDefaultDays
	}
	if days < 0 || days > ScopedMaxDays || !validScopedCommand(command) || len(label) > scopedMaxLabel || !cleanLabel(label) {
		return "", out, ErrScopedInvalid
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := p.ResolveDeviceTargetStrict(namespace, device)
	if err != nil {
		return "", out, err
	}
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return "", out, err
	}
	defer tx.Rollback()
	// Serialise per namespace so the live-token cap cannot be raced past.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "scoped-tokens:"+namespace); err != nil {
		return "", out, err
	}
	// Only a device that carries a stable id and a fingerprint can be scoped:
	// the token is pinned to that pair and stops working if either changes.
	var fp string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(fingerprint,'') FROM devices
 WHERE owner_namespace=$1 AND device_id=$2 AND uses_device_id`, namespace, id).Scan(&fp)
	if errors.Is(err, sql.ErrNoRows) || err == nil && fp == "" {
		return "", out, ErrDeviceNotFound
	}
	if err != nil {
		return "", out, err
	}
	var live int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM tokens
 WHERE namespace=$1 AND kind='scoped' AND revoked_at IS NULL AND expires_at>now()`, namespace).Scan(&live); err != nil {
		return "", out, err
	}
	if live >= scopedMaxLive {
		return "", out, ErrScopedLimit
	}
	raw := delegation.ScopedTokenPrefix + randHex(20)
	expires := time.Now().Add(time.Duration(days) * 24 * time.Hour)
	var tokenID int
	if err = tx.QueryRowContext(ctx, `INSERT INTO tokens(namespace,kind,hash,label,expires_at) VALUES ($1,'scoped',$2,$3,$4) RETURNING id`,
		namespace, HashToken(raw), label, expires).Scan(&tokenID); err != nil {
		return "", out, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO scoped_tokens(token_id,namespace,device_id,device_fp,command) VALUES ($1,$2,$3,$4,$5)`,
		tokenID, namespace, id, fp, command); err != nil {
		return "", out, err
	}
	if err = tx.Commit(); err != nil {
		return "", out, err
	}
	return raw, ScopedToken{ID: tokenID, Label: label, Device: id, Command: command, CreatedAt: time.Now(), ExpiresAt: expires}, nil
}

func (p *PGStore) ListScopedTokens(namespace string) ([]ScopedToken, error) {
	rows, err := p.db.Query(`SELECT t.id,COALESCE(t.label,''),s.device_id,s.command,t.created_at,t.expires_at,t.revoked_at
 FROM tokens t JOIN scoped_tokens s ON s.token_id=t.id WHERE t.namespace=$1 AND t.kind='scoped' ORDER BY t.id DESC`, namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ScopedToken{}
	for rows.Next() {
		var t ScopedToken
		var revoked sql.NullTime
		if err := rows.Scan(&t.ID, &t.Label, &t.Device, &t.Command, &t.CreatedAt, &t.ExpiresAt, &revoked); err != nil {
			return nil, err
		}
		if revoked.Valid {
			t.RevokedAt = &revoked.Time
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeScopedToken revokes one of the namespace's own scoped tokens. It reports
// false when there is no such live scoped token, so a mistyped id is not
// mistaken for a revocation.
func (p *PGStore) RevokeScopedToken(namespace string, id int) (bool, error) {
	res, err := p.db.Exec(`UPDATE tokens SET revoked_at=now() WHERE id=$1 AND namespace=$2 AND kind='scoped' AND revoked_at IS NULL`, id, namespace)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// resolveScoped completes ResolveAccess for a token whose kind is "scoped". The
// token's row has already been checked for revocation, expiry and a disabled
// account; what is left is that the device it was issued for is still the same
// device.
func (p *PGStore) resolveScoped(ctx context.Context, tokenID int, out delegation.Access) (delegation.Access, bool) {
	var deviceID, fp, command string
	err := p.db.QueryRowContext(ctx, `SELECT s.device_id,s.device_fp,s.command FROM scoped_tokens s
 JOIN devices d ON d.owner_namespace=s.namespace AND d.device_id=s.device_id AND d.uses_device_id AND d.fingerprint=s.device_fp
 WHERE s.token_id=$1 AND s.namespace=$2`, tokenID, out.Namespace).Scan(&deviceID, &fp, &command)
	if err != nil || out.ExpiresAt.IsZero() || command == "" {
		return delegation.Access{}, false
	}
	out.Delegated = true
	out.GrantID = "sc_" + strconv.Itoa(tokenID)
	out.Devices = []delegation.Device{{Namespace: out.Namespace, ID: deviceID, Fingerprint: fp}}
	out.ExecOnly = command
	return out, true
}
