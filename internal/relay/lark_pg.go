package relay

import "time"

// DeviceLarkApproval is the persisted per-device Feishu approval configuration.
type DeviceLarkApproval struct {
	Namespace       string    `json:"-"`
	Device          string    `json:"device"`
	ApprovalEnabled bool      `json:"approval_enabled"`
	PairingFromCard bool      `json:"pairing_from_card"`
	NotifyEmail     string    `json:"notify_email"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// ListLarkApproval returns every persisted Feishu approval configuration in a
// namespace. Devices without a row are deliberately absent; the portal applies
// the default-off behavior for individual device reads.
func (p *PGStore) ListLarkApproval(namespace string) ([]DeviceLarkApproval, error) {
	rows, err := p.db.Query(
		`SELECT namespace, device, approval_enabled, pairing_from_card, notify_email, updated_at
		   FROM device_lark_approval
		  WHERE namespace = $1
		  ORDER BY device`, namespace)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []DeviceLarkApproval{}
	for rows.Next() {
		var cfg DeviceLarkApproval
		if err := rows.Scan(&cfg.Namespace, &cfg.Device, &cfg.ApprovalEnabled,
			&cfg.PairingFromCard, &cfg.NotifyEmail, &cfg.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, cfg)
	}
	return out, rows.Err()
}

// UpsertLarkApproval persists a device's Feishu switches and notification
// recipient, returning the database's canonical row and timestamp.
func (p *PGStore) UpsertLarkApproval(cfg DeviceLarkApproval) (DeviceLarkApproval, error) {
	err := p.db.QueryRow(
		`INSERT INTO device_lark_approval
		   (namespace, device, approval_enabled, pairing_from_card, notify_email)
		 VALUES ($1,$2,$3,$4,$5)
		 ON CONFLICT (namespace, device) DO UPDATE
		   SET approval_enabled  = EXCLUDED.approval_enabled,
		       pairing_from_card = EXCLUDED.pairing_from_card,
		       notify_email      = EXCLUDED.notify_email,
		       updated_at        = now()
		 RETURNING namespace, device, approval_enabled, pairing_from_card, notify_email, updated_at`,
		cfg.Namespace, cfg.Device, cfg.ApprovalEnabled, cfg.PairingFromCard, cfg.NotifyEmail,
	).Scan(&cfg.Namespace, &cfg.Device, &cfg.ApprovalEnabled, &cfg.PairingFromCard,
		&cfg.NotifyEmail, &cfg.UpdatedAt)
	return cfg, err
}
