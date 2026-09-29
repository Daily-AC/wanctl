-- A contact address belongs to the signed-in identity, not to the user row or
-- to an application: an applicant has no user row yet and still has to be
-- told when they are let in. One row per identity. Only a row with
-- confirmed_at set is ever mailed; an unconfirmed row is an address carried
-- over from before this migration, kept to prefill the form that confirms it.
CREATE TABLE IF NOT EXISTS contact_emails (
    provider text NOT NULL,
    subject text NOT NULL,
    address text NOT NULL,
    confirmed_at timestamptz,
    PRIMARY KEY (provider, subject)
);

-- One row per confirmation link minted. The link carries the token; only its
-- SHA-256 is stored. The rows double as the send log the rate limits count,
-- so a used, superseded or failed link stays. failed_at marks a link whose
-- mail the SMTP server refused: it still counts against the identity's daily
-- allowance (each one cost an SMTP session), but not against the mailbox's
-- interval (nothing reached the mailbox).
CREATE TABLE IF NOT EXISTS email_confirmations (
    id serial PRIMARY KEY,
    token_hash bytea NOT NULL UNIQUE,
    provider text NOT NULL,
    subject text NOT NULL,
    login text NOT NULL DEFAULT '',
    address text NOT NULL,
    next text NOT NULL DEFAULT '/',
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    used_at timestamptz,
    failed_at timestamptz
);

CREATE INDEX IF NOT EXISTS email_confirmations_identity_idx
    ON email_confirmations (provider, subject, created_at DESC);
CREATE INDEX IF NOT EXISTS email_confirmations_address_idx
    ON email_confirmations (lower(address), created_at DESC);

-- Every address known so far comes across unconfirmed: a GitHub primary
-- address or one typed on the request form was never proven to reach the
-- person. The user row's address wins over an application's, and a later
-- application over an earlier one. users.email and access_requests.email
-- are no longer written after this; they stay so a rollback still finds them.
INSERT INTO contact_emails (provider, subject, address)
SELECT DISTINCT ON (provider, subject) provider, subject, address
  FROM (
        SELECT provider, provider_subject AS subject, email AS address, 0 AS rank, 0 AS id
          FROM users
         WHERE COALESCE(email, '') <> ''
        UNION ALL
        SELECT provider, subject, email, 1, id
          FROM access_requests
         WHERE email <> ''
       ) known
 ORDER BY provider, subject, rank, id DESC
ON CONFLICT (provider, subject) DO NOTHING;
