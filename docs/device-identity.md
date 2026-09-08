# Device identity

Every upgraded agent generates a random UUID v4 on first start and stores it in
`<WANCTL_CONFIG_DIR>/device_id` (the usual wanctl config directory when the
variable is unset). `wanctl id` prints this ID, the certificate fingerprint, and
the effective config directory. Concurrent starts publish the same ID; an invalid
existing file is an error rather than an invitation to silently reset identity.

The three concepts are independent:

- `device_id` identifies an installation. Relay routes, sharing grants, notification
  settings, audit associations, and controller pins use `namespace/device_id`.
- `--name` sets a display name (hostname by default, product model on Android).
  Portal aliases are display labels too. Both may repeat and change without
  replacing the installation.
- The certificate fingerprint authenticates the endpoint. Keeping the device ID
  while replacing the certificate still requires explicit identity confirmation.

Use `wanctl peers` to find IDs and labels, and `--target <ID>` or
`--target <namespace>/<ID>` to select an installation. A unique name or alias is
also accepted. Ambiguous names fail instead of selecting a device by registration
order. PostgreSQL-backed resolution includes offline devices in this check.
The portal uses full IDs for actions and shows a short ID beside duplicate labels.

## Upgrade

Upgrade the relay and portal together, then agents and controllers. Migration 007
renames the database routing column to `device_id`, adds mutable `display_name`
and migration metadata, and removes the alias uniqueness index. This is a schema
change: do not run old relay binaries against the migrated database. Back up the
database before deploying; rolling back requires restoring the corresponding
schema/data snapshot as well as the old binaries.

Legacy agents can keep their old routing key until upgraded. When a new agent
first reports its UUID, a legacy row is promoted only if its namespace, old name,
and certificate fingerprint all match. Promotion preserves the database row,
alias, sharing grants, notification settings/health, and audit associations in one
transaction. A same-name device with a different fingerprint creates a separate
record and inherits none of those associations. The old name cannot be
re-registered by an outdated agent once promoted.

New controllers resolve targets before checking trust. A promoted row exposes its
previous target so an existing pin can be copied to the UUID target. The copied
value is always the controller's stored fingerprint, never the relay's offered
value. Existing UUID pins are never overwritten. Old controllers can still target
UUIDs, but need an explicit initial pin because they do not migrate name-based pins.

Changing a device name during its first upgrade prevents the name-and-fingerprint
migration match; upgrade once with the original name before renaming it.
A legacy record already overwritten by the old same-name collision cannot recover
the overwritten device's identity automatically.

## Protocol compatibility

Agents send `device=<UUID>`, `device_id=<UUID>`, and `name=<display name>` on both
WebSocket registration and HTTP polling. Subsequent session, notification,
pairing, and deregistration requests use the UUID. `inst` remains an ephemeral
process instance marker, not the persistent identity.

For compatibility with existing consumers, `/peers` still returns canonical
routes in `devices`, and labels in `aliases`. Device-list JSON retains `name` as a
legacy alias for the canonical route and adds explicit `device_id`,
`display_name`, and `legacy_name` fields. Consumers must not use display labels as
map keys. Legacy records have no UUID `device_id` field until upgraded.

`GET /resolve?target=...` returns the authorized canonical `target` and, after a
legacy promotion, `legacy_target`. The same owner/ACL checks used for dialing
apply. An old relay returning 404 uses the older peers-based resolution path;
other failures do not fall back to guessing a target.

## Resetting and cloning

Keeping the config directory keeps the installation ID across upgrades and
renames. Deleting the directory creates a new installation. A full VM/config
clone also copies its device ID and certificate: before starting wanctl in an
independent clone, remove its copied `device_id`, `cert.pem`, and `key.pem`, then
re-enroll it as a new installation. The original device must retain its files.
This ID is not a MAC address, hardware serial number, or proof of ownership.

## Verification

`go test ./...` covers concurrent ID creation, same-name agents across both
transports, rename/restart continuity, and certificate mismatch rejection.

To run the real PostgreSQL migration, sharing, and pin-migration checks against a
disposable PostgreSQL server:

```sh
WANCTL_TEST_POSTGRES='postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable' \
  go test ./internal/relay -run TestDeviceIDPostgres -v
```

The integration test creates and removes its own schema. It requires permission
to create schemas, and does not modify existing schemas.
