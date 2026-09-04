package relay

import (
	"database/sql"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrAliasInvalid       = errors.New("alias_invalid")
	ErrAliasTaken         = errors.New("alias_taken")
	ErrAliasShadowsDevice = errors.New("alias_shadows_device")
	ErrDeviceNotFound     = errors.New("device_not_found")
)

type DeviceAlias struct {
	Name  string `json:"name"`
	Alias string `json:"alias"`
}

// DeviceAliasStore is optional so static-token relays keep their database-free
// behavior. PGStore implements it when the admin database is configured.
type DeviceAliasStore interface {
	SetDeviceAlias(namespace, device, alias string) (DeviceAlias, error)
	ResolveDeviceTarget(namespace, target string) (string, bool)
	ListDeviceAliases(namespace string) (map[string]string, error)
}

func normalizeDeviceAlias(alias string) (string, error) {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return "", nil
	}
	if !utf8.ValidString(alias) || utf8.RuneCountInString(alias) > 40 {
		return "", ErrAliasInvalid
	}
	for _, r := range alias {
		if r == '/' || unicode.IsControl(r) {
			return "", ErrAliasInvalid
		}
	}
	return alias, nil
}

func (p *PGStore) SetDeviceAlias(namespace, device, alias string) (DeviceAlias, error) {
	alias, err := normalizeDeviceAlias(alias)
	if err != nil {
		return DeviceAlias{}, err
	}
	if alias == "" {
		var out DeviceAlias
		err := p.db.QueryRow(
			`UPDATE devices SET alias = NULL
			  WHERE owner_namespace = $1 AND name = $2
			  RETURNING name, COALESCE(alias,'')`, namespace, device,
		).Scan(&out.Name, &out.Alias)
		if errors.Is(err, sql.ErrNoRows) {
			return DeviceAlias{}, ErrDeviceNotFound
		}
		return out, err
	}

	var exists, shadows, taken bool
	err = p.db.QueryRow(
		`SELECT
		   EXISTS (SELECT 1 FROM devices WHERE owner_namespace = $1 AND name = $2),
		   EXISTS (SELECT 1 FROM devices WHERE owner_namespace = $1 AND lower(name) = lower($3)),
		   EXISTS (SELECT 1 FROM devices
		            WHERE owner_namespace = $1 AND name <> $2
		              AND alias IS NOT NULL AND lower(alias) = lower($3))`,
		namespace, device, alias,
	).Scan(&exists, &shadows, &taken)
	if err != nil {
		return DeviceAlias{}, err
	}
	if !exists {
		return DeviceAlias{}, ErrDeviceNotFound
	}
	if shadows {
		return DeviceAlias{}, ErrAliasShadowsDevice
	}
	if taken {
		return DeviceAlias{}, ErrAliasTaken
	}

	var out DeviceAlias
	err = p.db.QueryRow(
		`UPDATE devices SET alias = $3
		  WHERE owner_namespace = $1 AND name = $2
		  RETURNING name, alias`, namespace, device, alias,
	).Scan(&out.Name, &out.Alias)
	if isAliasUniqueViolation(err) {
		return DeviceAlias{}, ErrAliasTaken
	}
	if errors.Is(err, sql.ErrNoRows) {
		return DeviceAlias{}, ErrDeviceNotFound
	}
	return out, err
}

func isAliasUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		pgErr.ConstraintName == "devices_owner_namespace_alias_key"
}

// ResolveDeviceTarget returns an exact device name first, then a
// case-insensitive alias match. Database errors leave the target unchanged.
func (p *PGStore) ResolveDeviceTarget(namespace, target string) (string, bool) {
	var name string
	err := p.db.QueryRow(
		`SELECT name FROM devices
		  WHERE owner_namespace = $1
		    AND (name = $2 OR (alias IS NOT NULL AND lower(alias) = lower($2)))
		  ORDER BY CASE WHEN name = $2 THEN 0 ELSE 1 END
		  LIMIT 1`, namespace, target,
	).Scan(&name)
	return name, err == nil
}

func (p *PGStore) ListDeviceAliases(namespace string) (map[string]string, error) {
	rows, err := p.db.Query(
		`SELECT name, alias FROM devices
		  WHERE owner_namespace = $1 AND alias IS NOT NULL`, namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, alias string
		if err := rows.Scan(&name, &alias); err != nil {
			return nil, err
		}
		out[name] = alias
	}
	return out, rows.Err()
}
