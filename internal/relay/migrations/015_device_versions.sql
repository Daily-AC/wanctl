ALTER TABLE devices
    ADD COLUMN wanctl_version text NOT NULL DEFAULT '',
    ADD COLUMN opencu_version text NOT NULL DEFAULT '',
    ADD COLUMN versions_reported_at timestamptz;
