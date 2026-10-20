package relay

import (
	"encoding/json"

	"wanctl/internal/release"
)

const maxDeviceVersionBytes = 128

// Reported versions are display metadata, never authority to install or execute.
func cleanDeviceVersion(value string) string {
	if len(value) > maxDeviceVersionBytes {
		return ""
	}
	for _, c := range value {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '+' || c == '_') {
			return ""
		}
	}
	return value
}

func jsonVersion(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

type deviceVersionStore interface {
	RecordDeviceVersions(namespace, device, wanctlVersion, opencuVersion string, reported bool) error
}

func (r *Relay) recordDeviceVersions(ns, device, wanctlVersion, opencuVersion string, reported bool) error {
	if store, ok := r.admin.(deviceVersionStore); ok {
		return store.RecordDeviceVersions(ns, device, cleanDeviceVersion(wanctlVersion), cleanDeviceVersion(opencuVersion), reported)
	}
	return nil
}

func (p *PGStore) RecordDeviceVersions(ns, device, wanctlVersion, opencuVersion string, reported bool) error {
	_, err := p.db.Exec(`UPDATE devices SET wanctl_version=$3, opencu_version=$4,
 versions_reported_at=CASE WHEN $5 THEN now() ELSE NULL END
 WHERE owner_namespace=$1 AND device_id=$2`, ns, device, cleanDeviceVersion(wanctlVersion), cleanDeviceVersion(opencuVersion), reported)
	return err
}

// Unknown includes development builds and an unavailable stable distribution.
// A newer beta keeps its installed label and is never marked as outdated.
func deviceVersionStatus(installed, stable string) string {
	cmp, err := release.CompareVersions(installed, stable)
	if err != nil {
		return "unknown"
	}
	if cmp < 0 {
		return "behind"
	}
	if cmp == 0 {
		return "latest"
	}
	return "ahead"
}
