"""Final bookkeeping after everything has been migrated."""

import logging

from synapse.server import HomeServer

from den2syn.dendrite import Dendrite, verify_equal

logger = logging.getLogger(__name__)


async def run(hs: HomeServer, dendrite: Dendrite) -> None:
    store = hs.get_datastores().main

    # The federation sender sends local events after this position to the
    # other servers in their rooms. A fresh database starts at -1, which would
    # send all migrated history, so start after it. The "federation" position
    # tracks worker replication, unrelated to events, so it's left alone.
    max_stream_ordering = store.get_room_max_stream_ordering()
    await store.update_federation_out_pos("events", max_stream_ordering)

    verify_equal("federation events position", max_stream_ordering, await store.get_federation_out_pos("events"))
    logger.info("Moved the federation events position to %d.", max_stream_ordering)
