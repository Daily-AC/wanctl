CREATE TABLE IF NOT EXISTS users (
    id serial PRIMARY KEY,
    -- Historical name from the original SSO integration; renamed to
    -- provider_subject by 002. Recreated here so a fresh database and a
    -- pre-migration production database converge on the same shape.
    feishu_open_id text NULL,
    namespace text NOT NULL,
    name text NULL,
    email text NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS devices (
    id serial PRIMARY KEY,
    owner_namespace text NOT NULL,
    name text NOT NULL,
    fingerprint text NULL,
    last_seen timestamptz NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS tokens (
    id serial PRIMARY KEY,
    namespace text NOT NULL,
    kind text NOT NULL DEFAULT 'access',
    hash text NOT NULL,
    label text NULL,
    expires_at timestamptz NULL,
    revoked_at timestamptz NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS acl (
    id serial PRIMARY KEY,
    owner_namespace text NOT NULL,
    device text NOT NULL,
    grantee_namespace text NOT NULL,
    perms text NOT NULL DEFAULT 'exec,read,write',
    created_at timestamptz NOT NULL DEFAULT now(),
    revoked_at timestamptz NULL
);

CREATE TABLE IF NOT EXISTS audit (
    id bigserial PRIMARY KEY,
    ts timestamptz NOT NULL DEFAULT now(),
    namespace text NULL,
    device text NULL,
    token_id integer NULL,
    event text NULL,
    bytes_up bigint NULL,
    bytes_down bigint NULL
);

CREATE TABLE IF NOT EXISTS doc_groups (
    id serial PRIMARY KEY,
    slug text NOT NULL,
    title text NOT NULL,
    position integer NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS docs (
    id serial PRIMARY KEY,
    group_id integer NULL,
    slug text NOT NULL,
    title text NOT NULL,
    body text NOT NULL,
    position integer NOT NULL DEFAULT 0,
    author_namespace text NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS device_lark_approval (
    namespace text NOT NULL,
    device text NOT NULL,
    approval_enabled boolean NOT NULL DEFAULT false,
    pairing_from_card boolean NOT NULL DEFAULT false,
    notify_email text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, device)
);

CREATE UNIQUE INDEX IF NOT EXISTS users_namespace_key ON users (namespace);
CREATE UNIQUE INDEX IF NOT EXISTS U&"users_fei\0073hu_open_id_key" ON users (U&"fei\0073hu_open_id");
CREATE UNIQUE INDEX IF NOT EXISTS devices_owner_namespace_name_key ON devices (owner_namespace, name);
CREATE UNIQUE INDEX IF NOT EXISTS tokens_hash_key ON tokens (hash);
CREATE UNIQUE INDEX IF NOT EXISTS doc_groups_slug_key ON doc_groups (slug);
CREATE UNIQUE INDEX IF NOT EXISTS docs_slug_key ON docs (slug);

CREATE INDEX IF NOT EXISTS devices_owner_namespace_idx ON devices (owner_namespace);
CREATE INDEX IF NOT EXISTS tokens_namespace_idx ON tokens (namespace);
CREATE INDEX IF NOT EXISTS acl_owner_namespace_idx ON acl (owner_namespace);
CREATE INDEX IF NOT EXISTS acl_grantee_namespace_idx ON acl (grantee_namespace);
CREATE INDEX IF NOT EXISTS audit_namespace_idx ON audit (namespace);
CREATE INDEX IF NOT EXISTS docs_group_id_idx ON docs (group_id);
