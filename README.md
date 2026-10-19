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

## Status

All 176 Synapse tables have stories.
All migration steps are implemented; see [PLAN.md](PLAN.md) for what remains before cutover.

For development, `scripts/dev-migrate.sh` loads the dump once and migrates into a recreated scratch database on every run.
Without `DENDRITE_MEDIA` set to Dendrite's media folder, it generates placeholder media files and points the scratch Dendrite database at them.

## Testing

```bash
# Story consistency, resolving each referenced Synapse function in the installed Synapse.
docker run --rm -v "$PWD":/den2syn:ro -w /den2syn --entrypoint python matrixdotorg/synapse:v1.163.0 -m unittest tests.test_stories

# End-to-end, from a Dendrite dump in integration-test/dendrite-dumps/.
SERVER_NAME=example.com integration-test/run-integration-test.sh
```
