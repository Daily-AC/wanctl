CREATE TABLE IF NOT EXISTS notify_webhook (
    namespace text PRIMARY KEY,
    url text NOT NULL,
    format text NOT NULL DEFAULT 'json',
    keyword text NOT NULL DEFAULT '',
    secret text NOT NULL DEFAULT '',
    on_approval boolean NOT NULL DEFAULT true,
    on_exec boolean NOT NULL DEFAULT false,
    on_lifecycle boolean NOT NULL DEFAULT true,
    on_security boolean NOT NULL DEFAULT true,
    exec_failures_only boolean NOT NULL DEFAULT true,
    include_detail boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS notify_health (
    namespace text NOT NULL,
    device text NOT NULL,
    attempted_at timestamptz NOT NULL,
    result text NOT NULL,
    http_status integer NOT NULL DEFAULT 0,
    provider_code text NOT NULL DEFAULT '',
    error text NOT NULL DEFAULT '',
    consecutive_failures integer NOT NULL DEFAULT 0,
    PRIMARY KEY (namespace, device)
);

CREATE TABLE IF NOT EXISTS device_notify (
    namespace text NOT NULL,
    device text NOT NULL,
    enabled boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, device)
);
