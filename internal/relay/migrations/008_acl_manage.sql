-- A share gives its grantee the owner's use of a device: exec, files and logs,
-- all still decided by the device's own mode and rules. Management -- the
-- device's approvals, rule set and mode -- is one switch the owner turns on per
-- share, and it is off unless they do.
-- See docs/adr/0007-shared-devices-inherit-owner-rights.md.
ALTER TABLE acl ADD COLUMN IF NOT EXISTS manage boolean NOT NULL DEFAULT false;
