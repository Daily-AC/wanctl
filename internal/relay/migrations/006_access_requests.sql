CREATE TABLE IF NOT EXISTS access_requests (
    id serial PRIMARY KEY,
    provider text NOT NULL,
    subject text NOT NULL,
    login text NOT NULL,
    note text NOT NULL DEFAULT '',
    status text NOT NULL DEFAULT 'pending',
    created_at timestamptz NOT NULL DEFAULT now(),
    decided_at timestamptz,
    decided_by text NOT NULL DEFAULT '',
    CHECK (status IN ('pending', 'approved', 'declined'))
);

-- One open application per account is the whole rate limit: a second one
-- cannot be filed while the first is waiting, so the queue is bounded by the
-- number of accounts that have ever asked, not by how often they press submit.
CREATE UNIQUE INDEX IF NOT EXISTS access_requests_open_key
    ON access_requests (provider, subject) WHERE status = 'pending';

CREATE INDEX IF NOT EXISTS access_requests_subject_idx
    ON access_requests (provider, subject, id DESC);
