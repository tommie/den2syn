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

from den2syn.dendrite import Dendrite, VerificationError, verify_equal

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

    # Clients keep their Dendrite sync token across the migration, and Synapse
    # accepts it. Synapse reads its 7th field, Dendrite's device list
    # position, as the to-device position, and deletes the device's messages
    # up to it as delivered. Starting Synapse's to-device stream after any
    # position Dendrite handed out means an old token can't acknowledge
    # migrated messages, so they're delivered instead.
    ((dendrite_max,),) = [tuple(r.values()) for r in dendrite.query(
        """
        SELECT GREATEST(
            (SELECT last_value FROM keyserver_key_changes_seq),
            (SELECT COALESCE(MAX(change_id), 0) FROM keyserver_key_changes))
        """)]
    await store.db_pool.execute(
        "den2syn_to_device_sequence", "SELECT setval('device_inbox_sequence', GREATEST(?, (SELECT last_value FROM device_inbox_sequence)))",
        dendrite_max)
    logger.info("Starting to-device messages after Dendrite's device list position %d.", dendrite_max)

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
    ((min_stream_id,),) = await store.db_pool.execute(
        "den2syn_verify_device_inbox_start", "SELECT MIN(stream_id) FROM device_inbox")
    if min_stream_id is not None and min_stream_id <= dendrite_max:
        raise VerificationError(
            f"to-device message stream ID {min_stream_id} isn't after Dendrite's device list position {dendrite_max}")
    logger.info("Migrated %d to-device messages to %d devices.", len(rows), len(by_device))
