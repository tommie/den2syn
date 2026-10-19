"""Migrates a Dendrite database into a fresh Synapse database.

This runs inside Synapse's Python environment (e.g. the matrixdotorg/synapse
image of the version in sources/synapse), because it writes through Synapse's
own storage code. That way Synapse keeps its derived tables and streams
consistent, instead of us re-implementing them. See stories.py for what
happens to each table.

Synapse's storage functions commit as they go, so there is no dry-run or
rollback. The target must be a fresh database; on failure, recreate it and
start over.
"""

import argparse
import logging
import sys

import psycopg2
import yaml
from twisted.internet import defer, reactor

from synapse.config.homeserver import HomeServerConfig
from synapse.server import HomeServer
from synapse.storage import DataStore
from synapse.storage.database import LoggingTransaction

from den2syn import stories, steps
from den2syn.stories import Kind

logger = logging.getLogger("den2syn")


class MigrationHomeServer(HomeServer):
    # The full main-process store, which can persist events. This is what
    # Synapse's update_synapse_database script uses as well.
    DATASTORE_CLASS = DataStore


async def _table_counts(hs: HomeServer) -> dict[str, int]:
    def txn(txn: LoggingTransaction) -> dict[str, int]:
        txn.execute("SELECT tablename FROM pg_tables WHERE schemaname = 'public'")
        tables = [row[0] for row in txn.fetchall()]
        counts = {}
        for table in tables:
            # Table names come from the catalog, not user input.
            txn.execute(f'SELECT COUNT(*) FROM "{table}"')
            counts[table] = txn.fetchone()[0]
        return counts

    return await hs.get_datastores().main.db_pool.runInteraction("den2syn_table_counts", txn)


def _check_fresh(counts: dict[str, int]) -> None:
    """Fails unless every table den2syn or Synapse would fill is empty."""

    used = sorted(
        table
        for table, count in counts.items()
        if count
        and stories.STORIES[table].kind != Kind.SYNAPSE
        and not stories.STORIES[table].seeded
    )
    if used:
        raise stories.StoryError(
            "the Synapse database is not fresh; these tables have rows: " + ", ".join(used)
        )


def _check_empty(counts: dict[str, int]) -> None:
    """Fails if a table whose story says EMPTY got rows during migration."""

    filled = sorted(
        table
        for table, count in counts.items()
        if count and stories.STORIES[table].kind == Kind.EMPTY
    )
    if filled:
        raise stories.StoryError(
            "tables that should be empty have rows: " + ", ".join(filled)
        )


async def migrate(hs: HomeServer, dendrite, check_only: bool) -> None:
    counts = await _table_counts(hs)
    stories.check_tables(set(counts))
    logger.info("All %d Synapse tables have a story.", len(counts))
    _check_fresh(counts)
    if check_only:
        return

    for step in steps.STEPS:
        logger.info("Step %s...", step.name)
        await step.run(hs, dendrite)

    counts = await _table_counts(hs)
    _check_empty(counts)
    for table, count in sorted(counts.items()):
        story = stories.STORIES[table]
        if story.kind in (Kind.MIGRATED, Kind.DERIVED):
            logger.info("%-50s %-8s %10d", table, story.kind.value, count)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("-v", "--verbose", action="store_true")
    parser.add_argument(
        "--config", type=argparse.FileType("r"), required=True,
        help="the target Synapse's homeserver.yaml")
    parser.add_argument(
        "--dendrite-db", required=True,
        help="libpq connection string for the Dendrite database")
    parser.add_argument(
        "--check-only", action="store_true",
        help="only check the table stories and that the Synapse database is fresh")
    args = parser.parse_args()

    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )

    config = HomeServerConfig()
    config.parse_config_dict(yaml.safe_load(args.config), "", "")
    hs = MigrationHomeServer(config.server.server_name, config=config, reactor=reactor)
    # Creates or upgrades Synapse's schema.
    hs.setup()
    # Instantiates storage classes so their background updates are registered.
    hs.get_storage_controllers()

    # Dendrite is only ever read, so a single read-only connection in the
    # reactor thread is fine. Blocking is acceptable in a batch tool.
    dendrite = psycopg2.connect(args.dendrite_db)
    dendrite.set_session(readonly=True)

    exit_code = 1

    async def run() -> None:
        nonlocal exit_code
        try:
            await migrate(hs, dendrite, args.check_only)
            exit_code = 0
        except Exception:
            logger.exception("Migration failed.")
        finally:
            reactor.stop()

    hs.get_clock().call_when_running(lambda: defer.ensureDeferred(run()))
    reactor.run()
    sys.exit(exit_code)


if __name__ == "__main__":
    main()
