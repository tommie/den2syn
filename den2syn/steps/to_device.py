"""Migrates undelivered to-device messages.

These carry room keys and key requests between devices, so dropping them
could leave recent messages undecryptable. Dendrite deletes a message once
the device acknowledges it, so everything left is undelivered.
"""

import collections
import json
import logging
from typing import Any

from synapse.server import HomeServer

from den2syn.dendrite import Dendrite, verify_equal

logger = logging.getLogger(__name__)


async def run(hs: HomeServer, dendrite: Dendrite) -> None:
    store = hs.get_datastores().main
    # Synapse deletes a device's inbox along with the device, so messages to
    # devices that no longer exist are dropped.
    rows = dendrite.query(
        """
        SELECT s.user_id, s.device_id, s.content
        FROM syncapi_send_to_device s
        JOIN userapi_devices d ON s.user_id = '@' || d.localpart || ':' || d.server_name AND s.device_id = d.device_id
        WHERE d.server_name = %s
        ORDER BY s.id
        """,
        (dendrite.server_name,),
    )

    by_device: dict[tuple[str, str], list[dict[str, Any]]] = collections.defaultdict(list)
    for r in rows:
        by_device[(r["user_id"], r["device_id"])].append(json.loads(r["content"]))

    # Each call takes at most one message per device, and gets the next
    # stream position. Sending the k-th message of every device in round k
    # keeps each device's messages in order, in as few calls as possible.
    rounds = max((len(m) for m in by_device.values()), default=0)
    for k in range(rounds):
        batch: dict[str, dict[str, dict[str, Any]]] = collections.defaultdict(dict)
        for (user_id, device_id), messages in by_device.items():
            if k < len(messages):
                batch[user_id][device_id] = messages[k]
        await store.add_local_messages_from_client_to_device_inbox(batch)

    got: dict[tuple[str, str], list[dict[str, Any]]] = collections.defaultdict(list)
    for user_id, device_id, message_json in await store.db_pool.execute(
        "den2syn_verify_device_inbox",
        "SELECT user_id, device_id, message_json FROM device_inbox ORDER BY stream_id",
    ):
        got[(user_id, device_id)].append(json.loads(message_json))
    verify_equal("to-device messages", dict(by_device), dict(got))
    logger.info("Migrated %d to-device messages to %d devices.", len(rows), len(by_device))
