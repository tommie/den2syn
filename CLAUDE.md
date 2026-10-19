# CLAUDE.md

Migrates a Dendrite homeserver's PostgreSQL database into a fresh Synapse database.
See README.md for the design and usage.

## Layout

- `den2syn/` — Python tool, run as `python -m den2syn`.
  - `stories.py` — the story of every Synapse table (`STORIES`, `Kind`, `check_tables`).
  - `steps/` — migration steps in run order (`STEPS`), one module per step; each MIGRATED story names one. Each step verifies its own results (`verify_equal`).
  - `dendrite.py` — read-only Dendrite access (`Dendrite.query`).
  - `__main__.py` — boots Synapse's storage (`MigrationHomeServer`), checks stories and freshness, runs steps.
- `tests/test_stories.py` — story consistency; resolves every `via` against the installed Synapse.
- `tests/client_test.py` — client-level checks of a running migrated Synapse against Dendrite's database; `scripts/dev-client-test.sh` runs it on the dev loop's result. `tests/test_client_test.py` round-trips its key backup decryption.
- `scripts/dev-migrate.sh` — dev loop: keeps a PostgreSQL container with the dump loaded, recreates the Synapse database and runs den2syn. Its config is in `/tmp/den2syn/dev/data`.
  Without `DENDRITE_MEDIA`, `scripts/dev_media_placeholders.py` generates placeholder media and rewrites the scratch Dendrite database's media hashes and sizes to match.
- `PLAN.md` — ordered checklist of steps.
- `integration-test/` — Docker Compose run from a real Dendrite dump and media folder (`dendrite-dumps/`, `dendrite-dumps/media/`; gitignored, real user data). Use `DENDRITE_MEDIA=$PWD/integration-test/dendrite-dumps/media scripts/dev-migrate.sh` for real media in the dev loop.
- `sources/` — submodules: `dendrite` (0.15.2, from element-hq) and `synapse` (v1.163.0, schema 94).

## Requirements from the user

- Start from Synapse, not Dendrite: every Synapse table must have a story saying how it ends up correct.
  What gets migrated is decided by what Synapse supports.
  Don't report Dendrite data that has no home in Synapse.
- Write through Synapse's own storage code (`persist_events`, `store_state_group`, store functions) instead of raw SQL, so Synapse keeps derived tables and streams consistent.
  Use `DatabasePool.simple_*` only where a Synapse function would lose data (e.g. it assigns new IDs), and say why in the story.

## Commands

Everything runs in the Synapse image matching `sources/synapse`:

```bash
scripts/dev-migrate.sh [--check-only]
scripts/dev-client-test.sh
docker run --rm -v "$PWD":/den2syn:ro -w /den2syn --entrypoint python matrixdotorg/synapse:v1.163.0 -m unittest tests.test_stories tests.test_client_test
SERVER_NAME=example.com integration-test/run-integration-test.sh
```

The tool needs a full `homeserver.yaml` (server name, signing key); generate one with `python -m synapse.app.homeserver --generate-config`.

## Gotchas

- Synapse needs PostgreSQL 14 or later.
- Dev and test Synapse configs come from `scripts/synapse_dev_config.py` (used by `scripts/dev-migrate.sh` and the Compose file). It patches YAML rather than using `sed`, since the generated layout changes between Synapse versions.
- Synapse functions commit as they go: no dry-run, the target must be fresh. Some tables are seeded with the schema (`seeded=True` stories, `Kind.SYNAPSE`).
- A fresh Synapse database has pending background updates that create indexes storage functions rely on (e.g. `create_profile` upserts on `profiles.full_user_id`). den2syn runs them first, then re-queues and runs the stats and user directory rebuilds (`_REBUILD_UPDATES`) after migrating.
- Synapse returns 404 for a profile with neither display name nor avatar; that's expected.
- `federation_stream_position` starts at -1; unless moved past migrated events, the federation sender sends all of history.
- Synapse returns 400 for thumbnails missing from `local_media_repository_thumbnails` (unless `dynamic_thumbnails`), so they must be generated.
- Dendrite keeps push rules (`m.push_rules`) and tags (`m.tag`) in account data; Synapse uses `push_rules*` and `room_tags`.
- `synapse.media` is a namespace package; `pkgutil.walk_packages` skips it.
- Dendrite's `roomserver_events.state_snapshot_nid` is the state *before* the event, except for initial room creation events in rooms made by older Dendrite versions, where it's the state after. A snapshot's blocks combine in order, last entry per key wins (`LoadStateAtSnapshot`).
- `persist_events` can't take a rejected event in a batch with others; persist it alone, like federation does.
- Synapse drops a room's current state when no local user is joined.
- Synapse accepts Dendrite's sync tokens, but reads their fields as different streams: Dendrite's device list position becomes the to-device position, which acknowledges (deletes) to-device messages. Hence the to-device sequence is moved past it. The room position differs too: Dendrite's is ahead of Synapse's, so Synapse lowers it to its current position (`bound_future_token`), and messages a device hadn't synced before cutover are silently skipped. The user chose having clients clear their cache (only devices `scripts/dendrite_unsynced_devices.sql` lists) over moving the events stream past Dendrite's (which would give `limited` timelines, but may make Element show notifications for old messages).
- Persisting events one at a time is ~30 ms/event; the rooms step batches them and builds linear contexts itself (`_linear_context`).

## Commits

Subject is a present-tense sentence ending in a period, e.g. "Breaks apart the migrations.go file.", followed by a body explaining why when non-trivial, and an `Assisted-by: LLM` trailer.
