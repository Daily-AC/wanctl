DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_name = 'users' AND column_name = 'feishu_open_id') THEN
        ALTER TABLE users RENAME COLUMN feishu_open_id TO provider_subject;
    END IF;
END $$;
ALTER TABLE users ADD COLUMN provider text NOT NULL DEFAULT 'header';
ALTER TABLE users ADD COLUMN role text NOT NULL DEFAULT 'user';

CREATE UNIQUE INDEX IF NOT EXISTS users_provider_provider_subject_key
    ON users (provider, provider_subject);

CREATE TABLE IF NOT EXISTS invites (
    id serial PRIMARY KEY,
    code_hash text UNIQUE,
    github_login text,
    invited_by text,
    created_at timestamptz NOT NULL DEFAULT now(),
    used_at timestamptz,
    used_by_namespace text,
    CHECK (code_hash IS NOT NULL OR github_login IS NOT NULL)
);

CREATE UNIQUE INDEX IF NOT EXISTS invites_pending_github_login_key
    ON invites (github_login) WHERE used_at IS NULL;
