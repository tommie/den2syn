"""Migrates users' reports of abusive events, for server admins to act on."""

import json
import logging

from synapse.server import HomeServer

from den2syn.dendrite import Dendrite, VerificationError, verify_equal

logger = logging.getLogger(__name__)


async def run(hs: HomeServer, dendrite: Dendrite) -> None:
    store = hs.get_datastores().main
    # Dendrite refers to the reporting user by its state key NID.
    reports = dendrite.query(
        """
        SELECT r.room_id, e.event_id, k.event_state_key AS user_id, rep.reason, rep.score, rep.received_ts
        FROM roomserver_reported_events rep
        JOIN roomserver_rooms r USING (room_nid)
        JOIN roomserver_events e USING (event_nid)
        JOIN roomserver_event_state_keys k ON k.event_state_key_nid = rep.reporting_user_nid
        ORDER BY rep.id
        """)

    want = []
    for r in reports:
        if await store.get_event(r["event_id"], allow_none=True, allow_rejected=True) is None:
            raise VerificationError(f"reported event {r['event_id']} in {r['room_id']} wasn't migrated")
        # Synapse keeps the request body as content. Dendrite only kept the
        # reason and score, with an absent one stored as empty or zero.
        content = {"reason": r["reason"], "score": r["score"]}
        await store.add_event_report(
            room_id=r["room_id"],
            event_id=r["event_id"],
            user_id=r["user_id"],
            reason=r["reason"],
            content=content,
            received_ts=r["received_ts"],
        )
        want.append((r["received_ts"], r["room_id"], r["event_id"], r["user_id"], r["reason"], content))

    # Synapse's report readers join on room stats, which den2syn rebuilds
    # after all steps, so read the table.
    got = await store.db_pool.simple_select_list(
        "event_reports", None, ("received_ts", "room_id", "event_id", "user_id", "reason", "content"),
        desc="den2syn_verify_event_reports")
    verify_equal(
        "event reports",
        sorted(want, key=repr),
        sorted(((ts, room, event, user, reason, json.loads(content)) for ts, room, event, user, reason, content in got), key=repr),
    )
    logger.info("Migrated %d event reports.", len(reports))
