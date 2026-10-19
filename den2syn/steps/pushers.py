"""Migrates push notification registrations (pushers).

Runs after rooms, so pushers start after the last migrated event instead of
pushing history.
"""

import json
import logging

from synapse.server import HomeServer

from den2syn.dendrite import Dendrite, verify_equal

logger = logging.getLogger(__name__)


async def run(hs: HomeServer, dendrite: Dendrite) -> None:
    store = hs.get_datastores().main
    # Dendrite ties a pusher to the device (session) that created it, like
    # Synapse ties it to a device and access token.
    pushers = dendrite.query(
        """
        SELECT p.localpart, p.kind, p.app_id, p.app_display_name, p.device_display_name, p.pushkey,
            p.pushkey_ts_ms, p.lang, p.data, p.profile_tag, d.device_id, d.access_token
        FROM userapi_pushers p
        LEFT JOIN userapi_devices d USING (localpart, server_name, session_id)
        WHERE p.server_name = %s
        ORDER BY p.id
        """,
        (dendrite.server_name,),
    )
    last_stream_ordering = store.get_room_max_stream_ordering()

    migrated = []
    for p in pushers:
        user_id = dendrite.user_id(p["localpart"])
        if p["device_id"] is None:
            # Synapse deletes pushers when their device logs out.
            logger.warning("Dropping %s pusher %s of %s, whose device no longer exists.",
                           p["app_id"], p["device_display_name"], user_id)
            continue

        token_id = await store.db_pool.simple_select_one_onecol(
            "access_tokens", {"token": p["access_token"]}, "id", desc="den2syn_pusher_token")
        await store.add_pusher(
            user_id=user_id,
            kind=p["kind"],
            app_id=p["app_id"],
            app_display_name=p["app_display_name"],
            device_display_name=p["device_display_name"],
            pushkey=p["pushkey"],
            # Despite the column name, Dendrite stores seconds.
            pushkey_ts=p["pushkey_ts_ms"] * 1000,
            lang=p["lang"],
            data=json.loads(p["data"]),
            last_stream_ordering=last_stream_ordering,
            profile_tag=p["profile_tag"] or "",
            device_id=p["device_id"],
            access_token_id=token_id,
        )
        migrated.append((user_id, p["app_id"], p["pushkey"], p["device_id"]))

    got = await store.db_pool.simple_select_list(
        "pushers", None, ("user_name", "app_id", "pushkey", "device_id"), desc="den2syn_verify_pushers")
    verify_equal("pushers", sorted(migrated), sorted(got))
    logger.info("Migrated %d pushers.", len(migrated))
