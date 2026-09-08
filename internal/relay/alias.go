package relay

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"wanctl/internal/transport"
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
			  WHERE owner_namespace = $1 AND device_id = $2
			  RETURNING device_id, COALESCE(alias,'')`, namespace, device,
		).Scan(&out.Name, &out.Alias)
		if errors.Is(err, sql.ErrNoRows) {
			return DeviceAlias{}, ErrDeviceNotFound
		}
		return out, err
	}

	var out DeviceAlias
	err = p.db.QueryRow(
		`UPDATE devices SET alias = $3
		  WHERE owner_namespace = $1 AND device_id = $2
		  RETURNING device_id, alias`, namespace, device, alias,
	).Scan(&out.Name, &out.Alias)
	if errors.Is(err, sql.ErrNoRows) {
		return DeviceAlias{}, ErrDeviceNotFound
	}
	return out, err
}

// ResolveDeviceTarget implements the legacy store interface. Strict callers
// use ResolveDeviceTargetStrict to distinguish an ambiguous label from failure.
func (p *PGStore) ResolveDeviceTarget(namespace, target string) (string, bool) {
	id, err := p.ResolveDeviceTargetStrict(namespace, target)
	return id, err == nil
}

// ResolveDeviceTargetStrict prefers the immutable route, then a unique name or
// alias. Include offline devices so a name does not silently change meaning.
func (p *PGStore) ResolveDeviceTargetStrict(namespace, target string) (string, error) {
	rows, err := p.db.Query(`SELECT device_id FROM devices WHERE owner_namespace=$1
 AND (device_id=$2 OR display_name=$2 OR lower(alias)=lower($2))
 ORDER BY CASE WHEN device_id=$2 THEN 0 ELSE 1 END`, namespace, target)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var matches []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		matches = append(matches, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(matches) > 0 && matches[0] == target && transport.ValidDeviceID(target) {
		return target, nil
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("ambiguous device name; use a device ID")
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	return "", ErrDeviceNotFound
}

func (p *PGStore) ListDeviceAliases(namespace string) (map[string]string, error) {
	rows, err := p.db.Query(
		`SELECT device_id, COALESCE(alias, NULLIF(display_name,''), device_id) FROM devices
		  WHERE owner_namespace = $1`, namespace)
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
