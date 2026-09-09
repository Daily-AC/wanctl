package relay

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"time"

	"wanctl/internal/transport"
)

// DeviceRegistrationStore atomically promotes old name-based records and all
// their associations. Errors fail registration instead of exposing a partial move.
type DeviceRegistrationStore interface {
	RegisterDevice(namespace, id, name, fingerprint string) (bool, error)
}

// registerDeviceID is separate from legacy registration: an old agent must never
// overwrite the identity record of an upgraded installation.
func (r *Relay) registerDeviceID(ns, id, name, fp string) (bool, error) {
	if !transport.ValidDeviceID(id) || name == "" || len(name) > 255 || !transport.ValidFingerprint(fp) {
		return false, fmt.Errorf("invalid device registration")
	}
	created := false
	if store, ok := r.admin.(DeviceRegistrationStore); ok {
		var err error
		created, err = store.RegisterDevice(ns, id, name, fp)
		if err != nil {
			return false, err
		}
	}
	return created, nil
}

func (p *PGStore) RegisterDevice(ns, id, name, fp string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "device-id:"+ns); err != nil {
		return false, err
	}
	var exists bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM devices WHERE owner_namespace=$1 AND device_id=$2)`, ns, id).Scan(&exists); err != nil {
		return false, err
	}
	if !exists {
		var old string
		err = tx.QueryRow(`SELECT device_id FROM devices WHERE owner_namespace=$1 AND device_id=$2 AND fingerprint=$3 AND NOT uses_device_id FOR UPDATE`, ns, name, fp).Scan(&old)
		if err != nil && err != sql.ErrNoRows {
			return false, err
		}
		if err == nil {
			if _, err = tx.Exec(`UPDATE devices SET device_id=$3, legacy_name=$2, uses_device_id=true WHERE owner_namespace=$1 AND device_id=$2`, ns, old, id); err != nil {
				return false, err
			}
			for _, q := range []string{
				`UPDATE acl SET device=$3 WHERE owner_namespace=$1 AND device=$2`,
				`UPDATE audit SET device=$3 WHERE namespace=$1 AND device=$2`,
				`UPDATE device_lark_approval SET device=$3 WHERE namespace=$1 AND device=$2`,
				`UPDATE device_notify SET device=$3 WHERE namespace=$1 AND device=$2`,
				`UPDATE notify_health SET device=$3 WHERE namespace=$1 AND device=$2`,
			} {
				if _, err = tx.Exec(q, ns, old, id); err != nil {
					return false, err
				}
			}
			exists = true
		}
	}
	result, err := tx.Exec(`INSERT INTO devices (owner_namespace,device_id,display_name,fingerprint,last_seen,uses_device_id)
 VALUES ($1,$2,$3,$4,now(),true)
 ON CONFLICT (owner_namespace,device_id) DO UPDATE SET display_name=EXCLUDED.display_name,
 fingerprint=EXCLUDED.fingerprint,last_seen=now()
 WHERE devices.uses_device_id`, ns, id, name, fp)
	if err != nil {
		return false, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return false, fmt.Errorf("device ID conflicts with a legacy record")
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	return !exists, nil
}

// liveLabels snapshots labels from the active registries without retaining
// extra metadata for disconnected agents.
func (r *Relay) liveLabels(ns string) map[string]string {
	labels := map[string]string{}
	r.mu.Lock()
	for _, a := range r.agents {
		if a.ns == ns {
			label := a.name
			if label == "" {
				label = a.device
			}
			labels[a.device] = label
		}
	}
	r.mu.Unlock()
	r.hmu.Lock()
	for _, a := range r.hagents {
		if a.ns == ns && time.Since(a.lastSeen) <= httpAgentTTL {
			label := a.name
			if label == "" {
				label = a.device
			}
			labels[a.device] = label
		}
	}
	r.hmu.Unlock()
	return labels
}

func (r *Relay) resolveLiveLabel(ns, target string) (string, bool) {
	labels := r.liveLabels(ns)
	if _, ok := labels[target]; ok && transport.ValidDeviceID(target) {
		return target, true
	}
	found := ""
	for id, label := range labels {
		if label == target {
			if found != "" {
				return "", false
			}
			found = id
		}
	}
	if found != "" {
		return found, true
	}
	return target, true
}

// handleResolve gives controllers the exact route before they look up TLS pins.
// Metadata is released only after the same owner/ACL check used for dialing.
func (r *Relay) handleResolve(w http.ResponseWriter, req *http.Request) {
	ns, ok := r.auth(w, req)
	if !ok {
		http.Error(w, "unauthorized", 401)
		return
	}
	key, auth, reason, ok := r.dialAllowedReason(ns, req.URL.Query().Get("target"))
	if !ok {
		if reason == "" {
			reason = "device unavailable or ambiguous; use its device ID"
		}
		http.Error(w, reason, 409)
		return
	}
	out := map[string]string{"target": key}
	if r.admin != nil {
		rows, err := r.admin.ListDevices(auth.OwnerNamespace)
		if err != nil {
			http.Error(w, "device lookup failed", 503)
			return
		}
		for _, row := range rows {
			if row["name"] != auth.Device || row["owner"] != auth.OwnerNamespace {
				continue
			}
			if legacy, _ := row["legacy_name"].(string); legacy != "" {
				out["legacy_target"] = auth.OwnerNamespace + "/" + legacy
			}
			break
		}
	}
	writeJSON(w, out)
}

// Legacy agents remain usable until upgraded, but cannot take a UUID route or
// resurrect a migrated hostname and detach it from its existing permissions.
func (r *Relay) allowLegacyRegistration(ns, device string) error {
	if device == "" || strings.Contains(device, "/") || transport.ValidDeviceID(device) {
		return fmt.Errorf("device ID registration required")
	}
	if p, ok := r.admin.(*PGStore); ok {
		var promoted bool
		err := p.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM devices WHERE owner_namespace=$1 AND uses_device_id AND legacy_name=$2)`, ns, device).Scan(&promoted)
		if err != nil {
			return err
		}
		if promoted {
			return fmt.Errorf("device already upgraded; use an agent with device ID support")
		}
	}
	return nil
}
