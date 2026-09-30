-- The owner's approval phone (ADR 0015): at most one device per namespace whose
-- wanctl app receives pending approvals. A row only names the device; whether
-- it is online, and whether it really is a phone running the app, the portal
-- checks every time it uses it.
CREATE TABLE IF NOT EXISTS approval_phone (
    namespace text PRIMARY KEY,
    device text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
