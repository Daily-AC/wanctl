-- Temporary controllers use the same token lifecycle as ordinary controllers.
-- Only hashes are stored; the browser ticket and controller bearer stay out of DB.
CREATE TABLE delegation_requests (
    id text PRIMARY KEY,
    ticket_hash text NOT NULL UNIQUE,
    token_hash text NOT NULL UNIQUE,
    label text NOT NULL,
    controller_fingerprint text NOT NULL,
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected')),
    namespace text NOT NULL DEFAULT '',
    token_id integer UNIQUE REFERENCES tokens(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    request_expires_at timestamptz NOT NULL,
    decided_at timestamptz
);
CREATE INDEX delegation_pending_idx ON delegation_requests (request_expires_at) WHERE status = 'pending';
CREATE INDEX delegation_namespace_idx ON delegation_requests (namespace);
CREATE INDEX delegation_retention_idx ON delegation_requests (created_at);

-- Retain the approved identity even after removal/rotation so it cannot silently
-- acquire a replacement device. ResolveAccess checks the current device record.
CREATE TABLE delegation_devices (
    grant_id text NOT NULL REFERENCES delegation_requests(id),
    namespace text NOT NULL,
    device_id text NOT NULL,
    fingerprint text NOT NULL,
    PRIMARY KEY (grant_id, device_id)
);

CREATE TABLE delegation_jobs (
    id text PRIMARY KEY,
    grant_id text NOT NULL REFERENCES delegation_requests(id),
    request_id text NOT NULL,
    payload_hash text NOT NULL,
    payload jsonb NOT NULL,
    state text NOT NULL DEFAULT 'running' CHECK (state IN ('running', 'done', 'failed', 'unknown')),
    result jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    UNIQUE (grant_id, request_id)
);
