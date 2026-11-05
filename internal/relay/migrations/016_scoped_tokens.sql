-- Scoped command tokens (docs/adr/0021-scoped-command-token.md): a credential
-- bound to one device and one literal command. The token itself is an ordinary
-- row in tokens (kind 'delegated', which earlier relays refuse; this table is what marks it scoped), so expiry, revocation and account disabling
-- come from the machinery tokens already have; only the scope lives here.
-- device_fp pins the device identity at issue time, so a re-enrolled device
-- under the same id does not inherit the token.
CREATE TABLE scoped_tokens (
    token_id integer PRIMARY KEY REFERENCES tokens(id),
    namespace text NOT NULL,
    device_id text NOT NULL,
    device_fp text NOT NULL,
    command text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX scoped_tokens_namespace_idx ON scoped_tokens (namespace);
