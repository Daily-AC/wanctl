-- Separate the canonical device ID from the mutable display name. Existing
-- rows temporarily retain their old routing key until the agent upgrades.
ALTER TABLE devices ADD COLUMN display_name text;
ALTER TABLE devices ADD COLUMN legacy_name text;
UPDATE devices SET display_name = name;
ALTER TABLE devices RENAME COLUMN name TO device_id;
ALTER INDEX devices_owner_namespace_name_key RENAME TO devices_owner_namespace_device_id_key;
ALTER TABLE devices ALTER COLUMN display_name SET NOT NULL;
ALTER TABLE devices ALTER COLUMN display_name SET DEFAULT '';
ALTER TABLE devices ADD COLUMN uses_device_id boolean NOT NULL DEFAULT false;
-- Names and aliases are labels now, and may repeat. Resolution must reject ambiguity.
DROP INDEX devices_owner_namespace_alias_key;
