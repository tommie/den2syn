# den2syn - Matrix Dendrite to Synapse Migration Tool

Migrates a Dendrite homeserver's PostgreSQL database into a fresh Synapse database.

Targets Dendrite 0.13.5 and Synapse v1.163.0 (schema 94), as found in `sources/`.

## Design

den2syn is a Python tool that runs inside Synapse's own environment and writes through Synapse's storage code: `persist_events` for room history, `store_state_group` for state, and the store functions behind the client APIs for everything else.
Synapse then fills its derived tables (DAG edges, auth chains, current state, memberships, search, streams) itself, rather than us re-implementing them.

### Table stories

Every table in Synapse's database has a *story* in [`den2syn/stories.py`](den2syn/stories.py), saying how it ends up correct after migration:

- **migrated**: den2syn writes rows from Dendrite data, through the named Synapse functions.
- **derived**: Synapse writes rows itself, as a side effect of a migrated table or in a background update on first start.
- **empty**: the table must be empty after migration, e.g. caches, short-lived flows and features Dendrite doesn't have.
- **synapse**: Synapse bookkeeping created with the schema, left untouched.

What gets migrated is decided by what Synapse supports.
Dendrite data with no home in Synapse is left behind without being reported.

The tool fails if a table lacks a story, or a story names a table that doesn't exist, so a Synapse upgrade can't silently skip new tables.
After migrating, it fails if any *empty* table got rows.

### No dry-run

Synapse's storage functions commit as they go, so there is no transaction to roll back.
The target must be a fresh database (only *synapse* tables may have rows); on failure, recreate it and start over.

## Usage

Run in the Synapse image matching `sources/synapse`, with the target Synapse's `homeserver.yaml`:

```bash
python -m den2syn --config /data/homeserver.yaml --dendrite-db 'postgres://user:pass@host/dendrite' \
    --dendrite-media /path/to/dendrite/media_api/base_path

# Only check the table stories and that the Synapse database is fresh.
python -m den2syn --config /data/homeserver.yaml --dendrite-db '...' --check-only
```

The `server_name` in `homeserver.yaml` must be the Dendrite server's name.

Synapse requires PostgreSQL 14 or later.

## Cutover runbook

The migration only reads Dendrite's database and media, so Dendrite stays intact for rolling back.

### Before the day

1. **Rehearse with recent data.**
   Copy a fresh `pg_dump` of Dendrite's database to `integration-test/dendrite-dumps/dendrite.dump`, and Dendrite's media folder (`media_api.base_path`) to `integration-test/dendrite-dumps/media/`.
   Run `scripts/dev-migrate.sh` with `DENDRITE_MEDIA` set to that folder, then `scripts/dev-client-test.sh` with `PASSWORD_<localpart>` and `RECOVERY_KEY_<localpart>` for at least one user.
   Every step checks its own result, so a successful run is the main evidence the real one will work.

2. **Prepare Synapse**, of the version in `sources/synapse` (v1.163.0), on PostgreSQL 14 or later.
   Upgrade Synapse only after migrating, since den2syn uses Synapse's internal storage code.
   Create an empty database, as Synapse requires:

   ```sql
   CREATE DATABASE synapse ENCODING 'UTF8' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0;
   ```

3. **Convert Dendrite's signing key**, so other servers keep trusting events and requests this server signed.
   Dendrite's key is the PEM file in `global.private_key`:

   ```bash
   python scripts/dendrite_signing_key.py < matrix_key.pem > homeserver.signing.key
   ```

   Old keys in Dendrite's `global.old_private_keys` go into Synapse's `old_signing_keys`.
   For ones with a private key file, convert it the same way, then run `export_signing_key -x --expiry-ts <expired_at> old.signing.key` in the Synapse image.
   For ones with only `public_key` and `key_id`, write the entry directly: `<key_id>: {key: <public_key>, expired_ts: <expired_at>}`.

4. **Write `homeserver.yaml`**, e.g. by generating one (`python -m synapse.app.homeserver --generate-config ...`), then:

   - `server_name`: Dendrite's `global.server_name`.
   - `public_baseurl`: the client URL users already have.
   - `database`: the empty database, with `name: psycopg2`.
   - `signing_key_path`: the converted key; `old_signing_keys` as above.
   - `media_store_path`: where Synapse should keep media; den2syn copies Dendrite's files there.
   - `max_upload_size`: at least Dendrite's `media_api.max_file_size_bytes`.
   - `password_config.pepper`: leave unset, or migrated password hashes won't match.
   - Registration (`enable_registration`, `registration_shared_secret`): to match Dendrite's `client_api` settings.
   - `serve_server_wellknown` and `public_baseurl`, if Dendrite served `.well-known` (`global.well_known_server_name`, `global.well_known_client_name`). Otherwise keep serving them as today.
   - `delete_stale_devices_after`: consider e.g. `1y`.
     Dendrite never removed abandoned sessions, which hold most of the undelivered to-device messages.
     Synapse runs it daily, based on each device's last seen time, which is migrated.

   Don't start Synapse yet: it must first see the database after migration.

### On the day

1. **Have everyone open their Matrix apps** shortly before, so their devices sync everything (see step 6).

2. **Stop Dendrite**, so nothing changes during the migration.
   Then list the devices that may not have synced every event:

   ```bash
   psql -f scripts/dendrite_unsynced_devices.sql 'postgres://dendrite:...@db/dendrite'
   ```

3. **Migrate**, in the Synapse image, with the config, signing key and media store mounted where `homeserver.yaml` says, and Dendrite's media read-only:

   ```bash
   docker run --rm -v "$PWD":/den2syn:ro -w /den2syn \
       -v /etc/synapse:/data -v /var/lib/synapse/media:/data/media_store \
       -v /var/lib/dendrite/media:/dendrite-media:ro \
       --entrypoint python matrixdotorg/synapse:v1.163.0 \
       -m den2syn --config /data/homeserver.yaml \
       --dendrite-db 'postgres://dendrite:...@db/dendrite' --dendrite-media /dendrite-media
   ```

   Run it as the user Synapse runs as (the image's default is UID 991), so it owns the media files.
   On failure, fix the cause, recreate the Synapse database, empty the media store, and run again.

4. **Start Synapse**, and check it with the client test:

   ```bash
   python -m tests.client_test --synapse http://synapse:8008 \
       --dendrite-db 'postgres://dendrite:...@db/dendrite' --dendrite-media /dendrite-media
   ```

5. **Point the reverse proxy at Synapse**: `/_matrix` and `/_synapse/client` to its listener, for both client and federation traffic (or port 8448, as Dendrite had it).

6. **Have users clear the cache of devices listed in step 2** that are still in use, e.g. Element's "Clear cache and reload" under Settings → Help & About.
   They stay logged in, with the same devices and keys.
   Clients keep their Dendrite sync token, which Synapse accepts but reads differently: Dendrite's room position is ahead of Synapse's, so Synapse lowers it to its current position.
   Messages a device hadn't synced before the cutover then never appear in its timeline, which it doesn't know has a gap.
   Clearing the cache makes the client do a full sync.
   Devices that had synced everything lose nothing, so they don't need it.
   The script errs towards listing devices: Dendrite updates a device's last seen time at most once a minute.
   An abandoned device that's listed only has the gap if it's ever used again; clearing its cache then fixes it.
   Waiting to-device messages (e.g. room keys) aren't affected: den2syn makes sure old tokens can't acknowledge them.

### Rolling back

Stop Synapse, point the proxy back, and start Dendrite.
Anything sent through Synapse in between is lost, and clients need their cache cleared again.

### What changes for users

- Unread counts and notification history start empty; read receipts are kept.
- Push notifications keep working, through the same pushers.
- Media from other servers is downloaded again on demand.
- Server-side features Dendrite didn't have become available, e.g. sliding sync for Element X.

## Status

All 176 Synapse tables have stories.
All migration steps are implemented; see [PLAN.md](PLAN.md) for what remains before cutover.

For development, `scripts/dev-migrate.sh` loads the dump once and migrates into a recreated scratch database on every run.
Without `DENDRITE_MEDIA` set to Dendrite's media folder, it generates placeholder media files and points the scratch Dendrite database at them.

## Testing

```bash
# Unit tests: story consistency (resolving each referenced Synapse function in the installed
# Synapse), and key backup decryption round trips.
docker run --rm -v "$PWD":/den2syn:ro -w /den2syn --entrypoint python matrixdotorg/synapse:v1.163.0 -m unittest tests.test_stories tests.test_client_test

# Client test against the dev loop's last migration.
scripts/dev-client-test.sh

# End-to-end, from a Dendrite dump and media folder in integration-test/dendrite-dumps/.
SERVER_NAME=example.com integration-test/run-integration-test.sh
```

The client test (`tests/client_test.py`) logs in as each local user with their most recently used Dendrite access token, and checks joined rooms, room history, device and cross-signing keys, the key backup, and avatar media against Dendrite's database.
It also checks that a client still holding a Dendrite sync token gets its waiting to-device messages.
Setting `PASSWORD_<localpart>` also logs in with that password, checking that Dendrite's password hash works in Synapse.
Setting `RECOVERY_KEY_<localpart>` also decrypts room keys from that user's backup and checks them against encrypted messages.
These are only read from the environment; in the integration test, also set `PASSWORD_USER=<localpart>` or `RECOVERY_KEY_USER=<localpart>`.
