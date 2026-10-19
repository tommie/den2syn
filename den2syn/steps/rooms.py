"""Migrates rooms and their event history.

Events are persisted in Dendrite's insertion order, which has every event
after its prev events, through Synapse's own persistence. Synapse computes
the state before each event from its prev events, like it does for new
events, so state groups are deltas of each other rather than full copies. We
check that this state matches Dendrite's snapshot for every event, and only
hand Synapse Dendrite's state when the prev events are missing.

Synapse then derives the DAG, auth chains, current state, memberships and
everything else from the persisted events.

Events are persisted in batches, so for linear history we build the context
from the prev event's state group ourselves, as Synapse can only look that up
for persisted events.
"""

import json
import logging
from typing import Any

from synapse.api.constants import EventTypes, Membership
from synapse.api.room_versions import KNOWN_ROOM_VERSIONS
from synapse.events import EventBase, make_event_from_dict
from synapse.events.snapshot import EventContext, UnpersistedEventContext
from synapse.server import HomeServer
from synapse.storage.controllers import StorageControllers
from synapse.types import RoomAlias, StateMap
from synapse.types.state import StateFilter

from den2syn.dendrite import Dendrite, VerificationError, verify_equal

logger = logging.getLogger(__name__)

# Events per persist_events call. Persisting one at a time takes about 30 ms
# per event, mostly per-transaction overhead.
_BATCH_SIZE = 500

# Synapse's RejectedReason.AUTH_ERROR. Dendrite only records that an event was
# rejected, and auth failure is why it rejects events.
_REJECTED_REASON = "auth_error"


async def run(hs: HomeServer, dendrite: Dendrite) -> None:
    rooms = dendrite.query(
        "SELECT room_nid, room_id, room_version, state_snapshot_nid, latest_event_nids FROM roomserver_rooms ORDER BY room_nid")
    published = {
        r["room_id"]
        for r in dendrite.query(
            "SELECT room_id FROM roomserver_published WHERE published AND appservice_id = ''")
    }

    for room in rooms:
        await _room(hs, dendrite, room, room["room_id"] in published)
    await _appservice_published(hs, dendrite)
    await _aliases(hs, dendrite)
    await _forgotten(hs, dendrite)


class _DendriteState:
    """Resolves Dendrite state snapshots of one room to state maps."""

    def __init__(self, dendrite: Dendrite, room_nid: int) -> None:
        self._dendrite = dendrite
        self._room_nid = room_nid
        # event_nid -> ((type, state_key), event_id), for the room's state events.
        self._state_events = {
            r["event_nid"]: ((r["type"], r["state_key"]), r["event_id"])
            for r in dendrite.query(
                """
                SELECT e.event_nid, e.event_id, t.event_type AS type, k.event_state_key AS state_key
                FROM roomserver_events e
                JOIN roomserver_event_types t USING (event_type_nid)
                JOIN roomserver_event_state_keys k USING (event_state_key_nid)
                WHERE e.room_nid = %s AND e.event_state_key_nid <> 0
                """,
                (room_nid,),
            )
        }
        self._snapshots = {
            r["state_snapshot_nid"]: r["event_nids"]
            for r in dendrite.query(
                """
                SELECT s.state_snapshot_nid, array_agg(n.event_nid ORDER BY b.ord, n.event_nid) AS event_nids
                FROM roomserver_state_snapshots s,
                    unnest(s.state_block_nids) WITH ORDINALITY AS b(state_block_nid, ord)
                    JOIN roomserver_state_block USING (state_block_nid),
                    unnest(event_nids) AS n(event_nid)
                WHERE s.room_nid = %s
                GROUP BY s.state_snapshot_nid
                """,
                (room_nid,),
            )
        }
        self._cache: dict[int, StateMap[str]] = {}

    def state(self, snapshot_nid: int) -> StateMap[str]:
        if snapshot_nid in self._cache:
            return self._cache[snapshot_nid]
        # Like Dendrite's LoadStateAtSnapshot: blocks are combined in the
        # snapshot's order, and the last entry for a state key wins. Within a
        # block, Dendrite sorts entries by event NID last, so the latest
        # event wins there. A snapshot without blocks has no row.
        out: dict[tuple[str, str], str] = {}
        for event_nid in self._snapshots.get(snapshot_nid, []):
            key, event_id = self._state_events[event_nid]
            out[key] = event_id
        self._cache[snapshot_nid] = out
        return out


async def _room(hs: HomeServer, dendrite: Dendrite, room: dict[str, Any], is_public: bool) -> None:
    room_id = room["room_id"]
    room_version = KNOWN_ROOM_VERSIONS.get(room["room_version"])
    if room_version is None:
        raise ValueError(f"{room_id}: room version {room['room_version']} isn't supported by Synapse")

    store = hs.get_datastores().main
    controllers = hs.get_storage_controllers()
    state_handler = hs.get_state_handler()
    state = _DendriteState(dendrite, room["room_nid"])

    events = dendrite.query(
        """
        SELECT e.event_nid, e.event_id, e.state_snapshot_nid, e.is_rejected, j.event_json
        FROM roomserver_events e
        JOIN roomserver_event_json j USING (event_nid)
        WHERE e.room_nid = %s
        ORDER BY e.event_nid
        """,
        (room["room_nid"],),
    )
    if not events:
        raise ValueError(f"{room_id}: no events")

    create = make_event_from_dict(json.loads(events[0]["event_json"]), room_version)
    if create.type != EventTypes.Create:
        raise ValueError(f"{room_id}: the first event is {create.type}, not the create event")
    await store.store_room(room_id, create.sender, is_public, room_version)

    # The state group after each non-outlier event handled so far. For a
    # rejected event, that's the state before it.
    state_after: dict[str, int] = {}
    # Events with contexts, waiting to be persisted in a batch.
    pending: list[tuple[EventBase, EventContext]] = []
    # (Dendrite snapshot, Synapse state group, includes the event) known to
    # hold the same state.
    verified: set[tuple[int, int, bool]] = set()
    # Membership of the latest member events, for checking current state.
    memberships: dict[str, str] = {}
    used_dendrite_state = 0
    after_state_snapshots = 0

    async def flush() -> None:
        if pending:
            await controllers.persistence.persist_events(pending)
            pending.clear()

    for i, row in enumerate(events):
        event = make_event_from_dict(json.loads(row["event_json"]), room_version)
        verify_equal(f"{room_id}: event ID of event {row['event_nid']}", row["event_id"], event.event_id)
        if event.type == EventTypes.Member:
            memberships[event.event_id] = event.membership

        if row["state_snapshot_nid"] == 0:
            # Dendrite doesn't know the state at the event, e.g. an auth event
            # fetched over federation.
            event.internal_metadata.outlier = True
            context = EventContext.for_outlier(controllers)
        else:
            dendrite_state = state.state(row["state_snapshot_nid"])
            # Dendrite's snapshot is normally the state before the event. But
            # older Dendrite versions stored the state after the event for the
            # events of the initial room creation, so the snapshot includes the
            # event itself.
            key = (event.type, event.state_key) if event.is_state() else None
            includes_event = key is not None and dendrite_state.get(key) == event.event_id
            if includes_event:
                after_state_snapshots += 1

            prev_event_ids = event.prev_event_ids()
            if len(prev_event_ids) == 1 and prev_event_ids[0] in state_after:
                # The common case of a linear history. Synapse would look up
                # the prev event's state group in the database, which requires
                # persisting one event at a time. This is what
                # StateHandler.calculate_context_info does when it finds that
                # state group.
                context = await _linear_context(controllers, event, state_after[prev_event_ids[0]])
            elif all(p in state_after for p in prev_event_ids):
                # Merging history; let Synapse resolve the state of the prev
                # events, which must be persisted for that.
                await flush()
                context = await state_handler.compute_event_context(event)
            else:
                # Synapse can't compute the state from prev events it doesn't
                # have, e.g. history from before a federated join.
                if includes_event:
                    raise ValueError(f"{room_id}: {event.event_id} has missing prev events and a state snapshot including itself")
                context = await state_handler.compute_event_context(
                    event, state_ids_before_event=dendrite_state, partial_state=False)
                used_dendrite_state += 1

            check = (row["state_snapshot_nid"], context.state_group_before_event, includes_event)
            if check not in verified:
                synapse_state = await controllers.state.get_state_ids_for_group(context.state_group_before_event)
                if includes_event:
                    synapse_state = {**synapse_state, key: event.event_id}
                _verify_state(f"{room_id}: state at {event.event_id}", dendrite_state, synapse_state)
                verified.add(check)

            if row["is_rejected"]:
                # Like Synapse's federation code: the context is computed as
                # usual, and persisting ignores a rejected event's state.
                context.rejected = _REJECTED_REASON
                state_after[event.event_id] = context.state_group_before_event
            else:
                state_after[event.event_id] = context.state_group

        if context.rejected:
            # Synapse's federation code persists a rejected event alone, so it
            # doesn't change the forward extremities and Synapse skips
            # calculating the state delta. In a batch with other events, that
            # calculation fails on the rejected event's state group.
            await flush()
            pending.append((event, context))
            await flush()
        else:
            pending.append((event, context))
            if len(pending) >= _BATCH_SIZE:
                await flush()

        if (i + 1) % 5000 == 0:
            logger.info("%s: handled %d of %d events.", room_id, i + 1, len(events))
    await flush()

    await _verify_room(hs, dendrite, room, state, events, memberships)
    logger.info(
        "%s: migrated %d events; %d used Dendrite's state because prev events were missing, "
        "%d had Dendrite snapshots of the state after the event.",
        room_id, len(events), used_dendrite_state, after_state_snapshots)


async def _linear_context(controllers: StorageControllers, event: EventBase, state_group_before: int) -> EventContext:
    """Returns the context of an event with a single prev event."""

    delta = {}
    if event.is_state():
        key = (event.type, event.state_key)
        delta = {key: event.event_id}
        replaces = (await controllers.state.get_state_ids_for_group(
            state_group_before, StateFilter.from_types([key]))).get(key)
        if replaces and replaces != event.event_id:
            event.unsigned["replaces_state"] = replaces

    # Creates the state group after a state event.
    return await UnpersistedEventContext(
        storage=controllers,
        state_group_before_event=state_group_before,
        state_group_after_event=None,
        state_delta_due_to_event=delta,
        prev_group_for_state_group_before_event=None,
        delta_ids_to_state_group_before_event=None,
        # Dendrite has full state for every room it knows.
        partial_state=False,
    ).persist(event)


def _verify_state(what: str, want: StateMap[str], got: StateMap[str]) -> None:
    diff = {
        f"{k[0]}/{k[1]}": {"dendrite": want.get(k), "synapse": got.get(k)}
        for k in sorted(want.keys() | got.keys())
        if want.get(k) != got.get(k)
    }
    if diff:
        raise VerificationError(f"{what} differs: {diff}")


async def _verify_room(
    hs: HomeServer,
    dendrite: Dendrite,
    room: dict[str, Any],
    state: _DendriteState,
    events: list[dict[str, Any]],
    memberships: dict[str, str],
) -> None:
    store = hs.get_datastores().main
    room_id = room["room_id"]

    want = state.state(room["state_snapshot_nid"])
    # Synapse drops a room's current state when no local user is joined.
    if not any(
        typ == EventTypes.Member and hs.is_mine_id(state_key) and memberships[event_id] == Membership.JOIN
        for (typ, state_key), event_id in want.items()
    ):
        want = {}
    current = await hs.get_storage_controllers().state.get_current_state_ids(room_id)
    _verify_state(f"{room_id}: current state", want, current)

    latest = {r["event_id"] for r in dendrite.query(
        "SELECT event_id FROM roomserver_events WHERE event_nid = ANY(%s)", (room["latest_event_nids"],))}
    verify_equal(f"{room_id}: forward extremities", sorted(latest), sorted(await store.get_latest_event_ids_in_room(room_id)))

    got = await store.db_pool.simple_select_onecol("events", {"room_id": room_id}, "event_id", desc="den2syn_verify_events")
    verify_equal(f"{room_id}: events", sorted(r["event_id"] for r in events), sorted(got))


async def _appservice_published(hs: HomeServer, dendrite: Dendrite) -> None:
    store = hs.get_datastores().main
    rows = dendrite.query(
        "SELECT room_id, appservice_id, network_id FROM roomserver_published WHERE published AND appservice_id <> ''")
    for r in rows:
        await store.set_room_is_public_appservice(r["room_id"], r["appservice_id"], r["network_id"], True)
    logger.info("Migrated %d rooms published by application services.", len(rows))


async def _aliases(hs: HomeServer, dendrite: Dendrite) -> None:
    store = hs.get_datastores().main
    rows = dendrite.query(
        "SELECT alias, room_id, creator_id FROM roomserver_room_aliases WHERE alias LIKE %s",
        ("%:" + dendrite.server_name,),
    )
    for r in rows:
        await store.create_room_alias_association(
            RoomAlias.from_string(r["alias"]), r["room_id"], [dendrite.server_name], r["creator_id"])
    logger.info("Migrated %d room aliases.", len(rows))


async def _forgotten(hs: HomeServer, dendrite: Dendrite) -> None:
    store = hs.get_datastores().main
    rows = dendrite.query(
        """
        SELECT r.room_id, k.event_state_key AS user_id
        FROM roomserver_membership m
        JOIN roomserver_rooms r USING (room_nid)
        JOIN roomserver_event_state_keys k ON k.event_state_key_nid = m.target_nid
        WHERE m.forgotten AND m.target_local
        """
    )
    for r in rows:
        await store.forget(r["user_id"], r["room_id"])
    logger.info("Migrated %d forgotten rooms.", len(rows))
