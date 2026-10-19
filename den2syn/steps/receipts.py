"""Migrates read receipts.

Runs after rooms, so Synapse can resolve the receipts' events to stream
orderings. m.fully_read markers are room account data, migrated with
account_data.
"""

import logging

from synapse.server import HomeServer

from den2syn.dendrite import Dendrite, verify_equal

logger = logging.getLogger(__name__)


async def run(hs: HomeServer, dendrite: Dendrite) -> None:
    store = hs.get_datastores().main
    # Receipts by remote users are included, since Synapse keeps those too.
    receipts = dendrite.query(
        "SELECT room_id, receipt_type, user_id, event_id, receipt_ts FROM syncapi_receipts ORDER BY id")

    for r in receipts:
        # Dendrite doesn't support threaded receipts, so all are unthreaded.
        await store.insert_receipt(
            r["room_id"], r["receipt_type"], r["user_id"], [r["event_id"]], None, {"ts": r["receipt_ts"]})

    got = await store.db_pool.simple_select_list(
        "receipts_linearized", None, ("room_id", "receipt_type", "user_id", "event_id", "thread_id"),
        desc="den2syn_verify_receipts")
    verify_equal(
        "receipts",
        sorted((r["room_id"], r["receipt_type"], r["user_id"], r["event_id"], None) for r in receipts),
        sorted(got),
    )
    logger.info("Migrated %d receipts.", len(receipts))
