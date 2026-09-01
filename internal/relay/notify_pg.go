package relay

import (
	"database/sql"
	"errors"
	"time"
)

// NotifyWebhook is an account-wide outbound webhook configuration.
type NotifyWebhook struct {
	Namespace        string    `json:"-"`
	URL              string    `json:"url,omitempty"`
	Format           string    `json:"format"`
	Keyword          string    `json:"keyword,omitempty"`
	Secret           string    `json:"-"`
	OnApproval       bool      `json:"on_approval"`
	OnExec           bool      `json:"on_exec"`
	OnLifecycle      bool      `json:"on_lifecycle"`
	OnSecurity       bool      `json:"on_security"`
	ExecFailuresOnly bool      `json:"exec_failures_only"`
	IncludeDetail    bool      `json:"include_detail"`
	UpdatedAt        time.Time `json:"updated_at,omitempty"`
}

// DeviceNotify is the default-off switch for one owned device.
type DeviceNotify struct {
	Namespace string    `json:"-"`
	Device    string    `json:"device"`
	Enabled   bool      `json:"enabled"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

// NotifyHealth is the last relay-side delivery result for a device. Account
// events and test notifications use relayNotifyHealthDevice.
type NotifyHealth struct {
	Namespace           string    `json:"-"`
	Device              string    `json:"device"`
	AttemptedAt         time.Time `json:"attempted_at"`
	Result              string    `json:"result"`
	HTTPStatus          int       `json:"http_status"`
	ProviderCode        string    `json:"provider_code,omitempty"`
	Error               string    `json:"error,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
}

// NotifyStore is deliberately separate from AdminStore so notification support
// does not widen every relay test double and non-Postgres deployment.
type NotifyStore interface {
	GetNotifyWebhook(namespace string) (NotifyWebhook, bool, error)
	UpsertNotifyWebhook(NotifyWebhook) (NotifyWebhook, error)
	DeleteNotifyWebhook(namespace string) error
	GetDeviceNotify(namespace, device string) (DeviceNotify, bool, error)
	UpsertDeviceNotify(DeviceNotify) (DeviceNotify, error)
	GetNotifyHealth(namespace, device string) (NotifyHealth, bool, error)
	RecordNotifyHealth(NotifyHealth) error
}

func defaultNotifyWebhook(namespace string) NotifyWebhook {
	return NotifyWebhook{
		Namespace: namespace, Format: "json", OnApproval: true,
		OnLifecycle: true, OnSecurity: true, ExecFailuresOnly: true,
	}
}

func (p *PGStore) GetNotifyWebhook(namespace string) (NotifyWebhook, bool, error) {
	cfg := defaultNotifyWebhook(namespace)
	err := p.db.QueryRow(
		`SELECT namespace, url, format, keyword, secret, on_approval, on_exec, on_lifecycle,
		        on_security, exec_failures_only, include_detail, updated_at
		   FROM notify_webhook WHERE namespace = $1`, namespace,
	).Scan(&cfg.Namespace, &cfg.URL, &cfg.Format, &cfg.Keyword, &cfg.Secret, &cfg.OnApproval, &cfg.OnExec,
		&cfg.OnLifecycle, &cfg.OnSecurity, &cfg.ExecFailuresOnly, &cfg.IncludeDetail, &cfg.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return cfg, false, nil
	}
	return cfg, err == nil, err
}

func (p *PGStore) UpsertNotifyWebhook(cfg NotifyWebhook) (NotifyWebhook, error) {
	err := p.db.QueryRow(
		`INSERT INTO notify_webhook
		   (namespace, url, format, keyword, secret, on_approval, on_exec, on_lifecycle, on_security, exec_failures_only, include_detail)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		 ON CONFLICT (namespace) DO UPDATE
		   SET url = EXCLUDED.url, format = EXCLUDED.format, keyword = EXCLUDED.keyword, secret = EXCLUDED.secret,
		       on_approval = EXCLUDED.on_approval, on_exec = EXCLUDED.on_exec,
		       on_lifecycle = EXCLUDED.on_lifecycle, on_security = EXCLUDED.on_security,
		       exec_failures_only = EXCLUDED.exec_failures_only, include_detail = EXCLUDED.include_detail, updated_at = now()
		 RETURNING namespace, url, format, keyword, secret, on_approval, on_exec, on_lifecycle,
		           on_security, exec_failures_only, include_detail, updated_at`,
		cfg.Namespace, cfg.URL, cfg.Format, cfg.Keyword, cfg.Secret, cfg.OnApproval, cfg.OnExec,
		cfg.OnLifecycle, cfg.OnSecurity, cfg.ExecFailuresOnly, cfg.IncludeDetail,
	).Scan(&cfg.Namespace, &cfg.URL, &cfg.Format, &cfg.Keyword, &cfg.Secret, &cfg.OnApproval, &cfg.OnExec,
		&cfg.OnLifecycle, &cfg.OnSecurity, &cfg.ExecFailuresOnly, &cfg.IncludeDetail, &cfg.UpdatedAt)
	return cfg, err
}

func (p *PGStore) DeleteNotifyWebhook(namespace string) error {
	_, err := p.db.Exec(`DELETE FROM notify_webhook WHERE namespace = $1`, namespace)
	return err
}

func (p *PGStore) GetDeviceNotify(namespace, device string) (DeviceNotify, bool, error) {
	cfg := DeviceNotify{Namespace: namespace, Device: device}
	err := p.db.QueryRow(
		`SELECT namespace, device, enabled, updated_at
		   FROM device_notify WHERE namespace = $1 AND device = $2`, namespace, device,
	).Scan(&cfg.Namespace, &cfg.Device, &cfg.Enabled, &cfg.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return cfg, false, nil
	}
	return cfg, err == nil, err
}

func (p *PGStore) UpsertDeviceNotify(cfg DeviceNotify) (DeviceNotify, error) {
	err := p.db.QueryRow(
		`INSERT INTO device_notify (namespace, device, enabled)
		 VALUES ($1,$2,$3)
		 ON CONFLICT (namespace, device) DO UPDATE
		   SET enabled = EXCLUDED.enabled, updated_at = now()
		 RETURNING namespace, device, enabled, updated_at`,
		cfg.Namespace, cfg.Device, cfg.Enabled,
	).Scan(&cfg.Namespace, &cfg.Device, &cfg.Enabled, &cfg.UpdatedAt)
	return cfg, err
}

func (p *PGStore) GetNotifyHealth(namespace, device string) (NotifyHealth, bool, error) {
	health := NotifyHealth{Namespace: namespace, Device: device}
	err := p.db.QueryRow(
		`SELECT namespace, device, attempted_at, result, http_status, provider_code, error, consecutive_failures
		   FROM notify_health WHERE namespace = $1 AND device = $2`, namespace, device,
	).Scan(&health.Namespace, &health.Device, &health.AttemptedAt, &health.Result,
		&health.HTTPStatus, &health.ProviderCode, &health.Error, &health.ConsecutiveFailures)
	if errors.Is(err, sql.ErrNoRows) {
		return health, false, nil
	}
	return health, err == nil, err
}

func (p *PGStore) RecordNotifyHealth(health NotifyHealth) error {
	_, err := p.db.Exec(
		`INSERT INTO notify_health
		   (namespace, device, attempted_at, result, http_status, provider_code, error, consecutive_failures)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		 ON CONFLICT (namespace, device) DO UPDATE
		   SET attempted_at = EXCLUDED.attempted_at, result = EXCLUDED.result,
		       http_status = EXCLUDED.http_status, provider_code = EXCLUDED.provider_code,
		       error = EXCLUDED.error,
		       consecutive_failures = CASE WHEN EXCLUDED.result = 'success' THEN 0
		         ELSE notify_health.consecutive_failures + 1 END`,
		health.Namespace, health.Device, health.AttemptedAt, health.Result,
		health.HTTPStatus, health.ProviderCode, health.Error, health.ConsecutiveFailures,
	)
	return err
}

// UpsertDeviceCreated is the registration variant used by a notification-aware
// relay. It preserves UpsertDevice's best-effort contract and reports whether
// this registration inserted a previously unknown owned device.
func (p *PGStore) UpsertDeviceCreated(namespace, name, fingerprint string) bool {
	var created bool
	err := p.db.QueryRow(
		`INSERT INTO devices (owner_namespace, name, fingerprint, last_seen)
		 VALUES ($1,$2,NULLIF($3,''),now())
		 ON CONFLICT (owner_namespace, name) DO UPDATE
		   SET last_seen = now(),
		       fingerprint = COALESCE(NULLIF(EXCLUDED.fingerprint,''), devices.fingerprint)
		 RETURNING (xmax = 0)`, namespace, name, fingerprint,
	).Scan(&created)
	return err == nil && created
}
