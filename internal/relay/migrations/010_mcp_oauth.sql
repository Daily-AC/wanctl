-- OAuth 2.1 for the hosted MCP endpoint. Two tables, both durable for the same
-- reason: a client that re-registers after every relay restart, or a user who is
-- logged out by one, is exactly the failure this whole flow exists to remove.
--
-- Clients register themselves (RFC 7591) and are not reviewed. A client_id on
-- its own grants nothing; access begins when a signed-in human approves a
-- request on the portal.
CREATE TABLE oauth_clients (
    id text PRIMARY KEY,
    secret_hash text NOT NULL DEFAULT '',
    name text NOT NULL,
    redirect_uris jsonb NOT NULL,
    auth_method text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- The refresh token itself is stored hashed, like every other credential here.
-- `grant_envelope` is the namespace token sealed with the relay's MCP seed
-- (internal/mcpauth), so a copy of this table is not a copy of anyone's device
-- access: the seed lives in the relay's environment, never in the database.
CREATE TABLE oauth_refresh_tokens (
    hash text PRIMARY KEY,
    client_id text NOT NULL REFERENCES oauth_clients(id),
    namespace text NOT NULL,
    grant_envelope text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz
);

CREATE INDEX oauth_refresh_namespace_idx ON oauth_refresh_tokens (namespace);
CREATE INDEX oauth_refresh_expiry_idx ON oauth_refresh_tokens (expires_at) WHERE revoked_at IS NULL;
