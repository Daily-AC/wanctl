-- One flag disables an account (security review S-05). Every credential is
-- resolved through a lookup that checks it, so setting it cuts the account off
-- and clearing it gives back exactly what was there: no token, grant or device
-- enrollment is revoked on the way.
ALTER TABLE users ADD COLUMN IF NOT EXISTS disabled_at timestamptz;
