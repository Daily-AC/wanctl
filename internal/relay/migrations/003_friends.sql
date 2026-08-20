CREATE TABLE IF NOT EXISTS friends (
    id serial PRIMARY KEY,
    requester_ns text NOT NULL,
    addressee_ns text NOT NULL,
    status text NOT NULL DEFAULT 'pending',
    created_at timestamptz NOT NULL DEFAULT now(),
    accepted_at timestamptz,
    CHECK (requester_ns <> addressee_ns)
);

CREATE UNIQUE INDEX IF NOT EXISTS friends_pair_key
    ON friends (LEAST(requester_ns, addressee_ns), GREATEST(requester_ns, addressee_ns));
