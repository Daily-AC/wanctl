ALTER TABLE devices ADD COLUMN IF NOT EXISTS alias text NULL;

CREATE UNIQUE INDEX IF NOT EXISTS devices_owner_namespace_alias_key
    ON devices (owner_namespace, lower(alias))
    WHERE alias IS NOT NULL;
