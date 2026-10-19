"""The story of every table in Synapse's database.

Each table in the target Synapse database has exactly one story, explaining
how it ends up correct after migration. `check_tables` enforces that the story
list matches the live schema, so a Synapse upgrade that adds a table fails
loudly until someone writes its story.

What we migrate is driven by what Synapse supports, not by what Dendrite has:
Dendrite data without a home in Synapse is silently left behind.
"""

import enum
from dataclasses import dataclass


class Kind(enum.Enum):
    # den2syn writes rows based on Dendrite data, using `via`.
    MIGRATED = "migrated"
    # Synapse writes rows itself, as a side effect of something den2syn calls,
    # or in a background update queued on a fresh database.
    DERIVED = "derived"
    # The table must be empty after migration. Synapse fills it during normal
    # operation, if at all.
    EMPTY = "empty"
    # Synapse bookkeeping created along with the schema. den2syn leaves it as
    # it is.
    SYNAPSE = "synapse"


@dataclass(frozen=True)
class Story:
    kind: Kind
    note: str
    # The den2syn step responsible for a MIGRATED table.
    step: str = ""
    # Dendrite tables read for a MIGRATED table.
    source: tuple[str, ...] = ()
    # Synapse functions (or background updates) that write the table.
    via: tuple[str, ...] = ()
    # Synapse creates rows along with the schema, which den2syn then updates.
    # Such tables have rows even in a fresh database.
    seeded: bool = False


def migrated(
    step: str, source: tuple[str, ...], via: tuple[str, ...], note: str, seeded: bool = False
) -> Story:
    return Story(Kind.MIGRATED, note, step=step, source=source, via=via, seeded=seeded)


def derived(via: tuple[str, ...], note: str) -> Story:
    return Story(Kind.DERIVED, note, via=via)


def empty(note: str) -> Story:
    return Story(Kind.EMPTY, note)


def synapse(note: str) -> Story:
    return Story(Kind.SYNAPSE, note)


# Reused explanations.
_PERSIST = ("EventsPersistenceStorageController.persist_events",)
_PERSISTED = "Written by Synapse when den2syn persists the room's events."
_STATE_GROUPS = ("StateHandler.compute_event_context", "UnpersistedEventContext.persist")
_STATS = ("background update populate_stats_*, re-run by den2syn after migrating",)
_USER_DIR = ("background update populate_user_directory_*, re-run by den2syn after migrating",)
_DEVICE_UPDATES = ("DeviceHandler.notify_device_update", "E2eKeysHandler.upload_keys_for_user")
_NO_APPSERVICES = "No application services are migrated; they are re-registered through Synapse config."
_TRANSIENT = "Short-lived state; clients start new flows after migration."
_FEDERATION_CACHE = "Federation state that Synapse rebuilds as it talks to other servers."
_SLIDING_SYNC_CONN = "Per-connection sliding sync state; clients reconnect from scratch."

STORIES: dict[str, Story] = {
    "access_tokens": migrated(
        "users", ("userapi_devices",), ("RegistrationStore.add_access_token_to_user",),
        "Keeps existing client sessions logged in, one token per device."),
    "account_data": migrated(
        "account_data", ("userapi_account_datas",), ("AccountDataStore.add_account_data_for_user",),
        "Global account data, except m.push_rules, which Synapse keeps in push_rules. "
        "Includes secret storage and cross-signing secrets, which key recovery needs."),
    "account_validity": empty("Account validity is a Synapse-only feature, configured after migration."),
    "application_services_state": empty(_NO_APPSERVICES),
    "application_services_txns": empty(_NO_APPSERVICES),
    "applied_module_schemas": synapse("Schema bookkeeping for Synapse modules."),
    "applied_schema_deltas": synapse("Schema bookkeeping."),
    "appservice_room_list": migrated(
        "rooms", ("roomserver_published",), ("RoomStore.set_room_is_public_appservice",),
        "Rooms published to the directory by an application service."),
    "appservice_stream_position": synapse("Singleton stream position; nothing to process without appservices."),
    "background_updates": synapse(
        "den2syn runs the updates a fresh database has pending before migrating, like Synapse's first "
        "start would, since storage functions rely on the indexes they create. Afterwards it queues "
        "and runs the stats and user directory rebuilds again over the migrated data."),
    "blocked_rooms": empty("Admin moderation state with no counterpart to carry over."),
    "cache_invalidation_stream_by_instance": synapse("Worker cache invalidation stream."),
    "current_state_delta_stream": derived(_PERSIST, _PERSISTED),
    "current_state_events": derived(
        _PERSIST, "Computed by Synapse from the state of the room's forward extremities."),
    "dehydrated_devices": empty("Dendrite has no dehydrated devices; clients create new ones."),
    "delayed_events": empty("Dendrite has no delayed events (MSC4140)."),
    "delayed_events_stream_pos": synapse("Singleton stream position."),
    "deleted_pushers": empty("Stream of pusher deletions for workers; nothing has been deleted yet."),
    "destination_rooms": empty(_FEDERATION_CACHE),
    "destination_rooms_sticky_events_backlog": empty(_FEDERATION_CACHE),
    "destinations": empty(_FEDERATION_CACHE),
    "device_auth_providers": empty("SSO login provenance; Dendrite only supports password login."),
    "device_federation_inbox": empty("Deduplication of incoming to-device EDUs. " + _FEDERATION_CACHE),
    "device_federation_outbox": empty("Outgoing to-device EDUs. " + _FEDERATION_CACHE),
    "device_inbox": migrated(
        "to_device", ("syncapi_send_to_device",),
        ("DeviceInboxWorkerStore.add_local_messages_from_client_to_device_inbox",),
        "Undelivered to-device messages for migrated devices, in order. These carry room keys, so "
        "dropping them could make recent messages undecryptable. Messages to deleted devices are "
        "dropped, since Synapse deletes a device's inbox with the device."),
    "device_lists_changes_converted_stream_position": synapse("Singleton stream position."),
    "device_lists_changes_in_room": derived(
        _DEVICE_UPDATES,
        "Expected empty: keys are uploaded before rooms exist. Other servers in shared rooms already "
        "have these device lists from Dendrite."),
    "device_lists_changes_in_room_max_pruned_stream_id": synapse("Singleton stream position."),
    "device_lists_outbound_last_success": derived(
        ("FederationSender",), "Written as device list updates reach other servers."),
    "device_lists_outbound_pokes": derived(
        _DEVICE_UPDATES,
        "Expected empty: keys are uploaded before rooms exist. Other servers in shared rooms already "
        "have these device lists from Dendrite."),
    "device_lists_remote_cache": empty("Remote device lists are fetched again from their servers."),
    "device_lists_remote_extremeties": empty("Remote device lists are fetched again from their servers."),
    "device_lists_remote_pending": empty("Remote device lists are fetched again from their servers."),
    "device_lists_remote_resync": empty("Remote device lists are fetched again from their servers."),
    "device_lists_stream": derived(
        _DEVICE_UPDATES, "Written as device and cross-signing keys are uploaded."),
    "devices": migrated(
        "users", ("userapi_devices",), ("DeviceStore.store_device", "DatabasePool.simple_update_one"),
        "Device ID and display name. Last seen IP, user agent and timestamp are updated directly. "
        "Synapse also stores each cross-signing key as a hidden device."),
    "e2e_cross_signing_keys": migrated(
        "e2e_keys", ("keyserver_cross_signing_keys", "keyserver_cross_signing_sigs"),
        ("E2eKeysHandler.upload_signing_keys_for_user",),
        "Master, self-signing and user-signing keys. Dendrite stores only the public keys, so the JSON "
        "is rebuilt, with the master key's signatures on the other two from Dendrite's signature table."),
    "e2e_cross_signing_signatures": migrated(
        "e2e_keys", ("keyserver_cross_signing_sigs",),
        ("E2eKeysHandler.upload_signatures_for_device_keys",),
        "Uploaded one at a time, letting Synapse verify them. Dendrite's table also has signatures by "
        "and on replaced keys and deleted devices, which Synapse rejects; they're logged and dropped."),
    "e2e_device_keys_json": migrated(
        "e2e_keys", ("keyserver_device_keys",), ("E2eKeysHandler.upload_keys_for_user",),
        "Device identity keys of migrated devices; without them, other devices can't encrypt to them."),
    "e2e_fallback_keys_json": empty(
        "Dendrite doesn't store fallback keys; clients upload new ones when "
        "device_unused_fallback_key_types shows none."),
    "e2e_one_time_keys_json": migrated(
        "e2e_keys", ("keyserver_one_time_keys",), ("E2eKeysHandler.upload_keys_for_user",),
        "Unclaimed one-time keys, so other devices can start Olm sessions."),
    "e2e_room_keys": migrated(
        "e2e_keys", ("userapi_key_backups",), ("EndToEndRoomKeyStore.add_e2e_room_keys",),
        "Server-side key backup contents."),
    "e2e_room_keys_versions": migrated(
        "e2e_keys", ("userapi_key_backup_versions",),
        ("DatabasePool.simple_insert",),
        "Inserted directly, because create_e2e_room_keys_version numbers versions per user, while "
        "Dendrite numbers them globally, and clients remember the version they back up to."),
    "erased_users": empty("GDPR erasure is a Synapse-only feature."),
    "event_auth": derived(_PERSIST, _PERSISTED),
    "event_auth_chain_links": derived(_PERSIST, _PERSISTED),
    "event_auth_chain_to_calculate": derived(
        _PERSIST, "Only used when an auth chain can't be computed at persist time; expected empty."),
    "event_auth_chains": derived(_PERSIST, _PERSISTED),
    "event_backward_extremities": derived(
        _PERSIST, "Events whose prev_events Dendrite never had, e.g. history from before a federated join."),
    "event_edges": derived(_PERSIST, _PERSISTED),
    "event_expiry": derived(_PERSIST, "Only for MSC2228 self-destructing events."),
    "event_failed_pull_attempts": empty(_FEDERATION_CACHE),
    "event_forward_extremities": derived(_PERSIST, _PERSISTED),
    "event_json": derived(_PERSIST, "The event JSON exactly as Dendrite received it."),
    "event_labels": derived(_PERSIST, "Only for MSC2326 labelled events."),
    "event_push_actions": empty(
        "Synapse computes push actions before persisting an event, not as part of it, so historic "
        "notifications and unread counts start at zero. Receipts are migrated."),
    "event_push_actions_staging": empty("Transient staging for push actions."),
    "event_push_summary": empty("Summarizes event_push_actions, which starts empty."),
    "event_push_summary_last_receipt_stream_id": synapse("Singleton stream position."),
    "event_push_summary_stream_ordering": synapse("Singleton stream position."),
    "event_relations": derived(_PERSIST, _PERSISTED),
    "event_reports": empty("Abuse reports aren't carried over."),
    "event_search": derived(_PERSIST, "Full-text index of unencrypted messages."),
    "event_to_state_groups": derived(
        _PERSIST, "Points at the state groups den2syn builds from Dendrite's state snapshots."),
    "event_txn_id_device_id": empty("Deduplicates client retries for a short time."),
    "events": derived(_PERSIST, "Persisted room by room, in topological order."),
    "ex_outlier_stream": derived(_PERSIST, "Only written when an outlier is later persisted as a regular event."),
    "federation_inbound_events_staging": empty(_FEDERATION_CACHE),
    "federation_stream_position": migrated(
        "finalize", (), ("StreamWorkerStore.update_federation_out_pos",),
        "The events position is moved past the migrated events, or Synapse would send all local "
        "history to other servers. The federation position tracks worker replication and is left alone.",
        seeded=True),
    "ignored_users": derived(
        ("AccountDataStore.add_account_data_for_user",), "Updated from m.ignored_user_list account data."),
    "instance_map": synapse("Worker instance names."),
    "local_current_membership": derived(_PERSIST, _PERSISTED),
    "local_media_repository": migrated(
        "media", ("mediaapi_media_repository",), ("MediaStorage.store_file", "MediaRepositoryStore.store_local_media"),
        "Media uploaded by local users, copied with the same media IDs so mxc:// URIs keep working. "
        "Files are checked against Dendrite's hash and size, and migration fails if any is missing."),
    "local_media_repository_thumbnails": derived(
        ("MediaRepository._generate_thumbnails",),
        "Generated by Synapse's thumbnailer from the copied files. Synapse doesn't generate missing "
        "thumbnails on request unless dynamic_thumbnails is set."),
    "local_media_repository_url_cache": empty("URL preview cache."),
    "login_tokens": empty(_TRANSIENT),
    "monthly_active_users": empty("Only tracked when MAU limits are configured; filled as users are active."),
    "msc4242_state_dag_edges": empty("Only for MSC4242 room versions, which Dendrite doesn't support."),
    "msc4242_state_dag_forward_extremities": empty("Only for MSC4242 room versions, which Dendrite doesn't support."),
    "open_id_tokens": empty("OpenID tokens expire within an hour. " + _TRANSIENT),
    "partial_state_events": empty("Dendrite has full state for every room it knows."),
    "partial_state_rooms": empty("Dendrite has full state for every room it knows."),
    "partial_state_rooms_servers": empty("Dendrite has full state for every room it knows."),
    "per_user_experimental_features": empty("Admin toggles for Synapse experimental features."),
    "presence_stream": empty("Presence is ephemeral."),
    "profile_updates": derived(("ProfileWorkerStore.set_profile_field",), "Stream of profile changes."),
    "profile_updates_per_user": derived(("ProfileWorkerStore.set_profile_field",), "Stream of profile changes."),
    "profiles": migrated(
        "users", ("userapi_profiles",),
        ("ProfileWorkerStore.create_profile", "ProfileWorkerStore.set_profile_field"),
        "Display name and avatar URL. Every user gets a row, like Synapse's own registration does."),
    "push_rules": migrated(
        "account_data", ("userapi_account_datas",),
        ("PushRulesWorkerStore.add_push_rule", "PushRulesHandler.set_rule_attr"),
        "From m.push_rules account data: user-defined rules in Dendrite's order, and actions of default "
        "rules the user changed. Dendrite stores the full rule set, including its defaults from when "
        "the account was created, so values matching any default Dendrite has shipped "
        "(dendrite_default_push_rules.json) count as unchanged, and actions are compared by meaning, "
        "not text. Actions with duplicate tweaks were written by a buggy client or server and are "
        "ignored. Default rules Synapse doesn't have are dropped."),
    "push_rules_enable": migrated(
        "account_data", ("userapi_account_datas",), ("PushRulesHandler.set_rule_attr",),
        "Enabled flags that differ from Synapse's defaults, from m.push_rules account data."),
    "push_rules_stream": derived(("PushRulesWorkerStore.add_push_rule", "PushRulesWorkerStore.set_push_rule_enabled"), "Stream of push rule changes."),
    "pusher_throttle": empty("Transient rate limiting state for pushers."),
    "pushers": migrated(
        "pushers", ("userapi_pushers", "userapi_devices"), ("PusherStore.add_pusher",),
        "Starts at the last migrated event, so historic events aren't pushed. Linked to the device and "
        "access token that created it; pushers of devices that no longer exist are dropped, since "
        "Synapse deletes pushers on logout."),
    "quarantined_media_changes": empty("Admin moderation state with no counterpart to carry over."),
    "ratelimit_override": empty("Admin configuration with no counterpart to carry over."),
    "receipts_graph": derived(("ReceiptsWorkerStore.insert_receipt",), "Written with each receipt."),
    "receipts_linearized": migrated(
        "receipts", ("syncapi_receipts",), ("ReceiptsWorkerStore.insert_receipt",),
        "Read receipts, after events so Synapse can resolve their stream orderings."),
    "received_transactions": empty(_FEDERATION_CACHE),
    "redactions": derived(_PERSIST, _PERSISTED),
    "refresh_tokens": empty("Dendrite doesn't issue refresh tokens."),
    "registration_tokens": migrated(
        "users", ("userapi_registration_tokens",), ("RegistrationStore.create_registration_token",),
        "Use counts are updated directly, because create_registration_token starts them at zero."),
    "rejections": derived(_PERSIST, "Events Dendrite marked rejected are persisted as rejected."),
    "remote_media_cache": empty("Remote media is downloaded again from its origin on demand."),
    "remote_media_cache_thumbnails": empty("Remote media is downloaded again from its origin on demand."),
    "room_account_data": migrated(
        "account_data", ("userapi_account_datas",), ("AccountDataStore.add_account_data_to_room",),
        "Per-room account data, except m.tag, which Synapse keeps in room_tags."),
    "room_alias_servers": migrated(
        "rooms", ("roomserver_room_aliases",), ("DirectoryStore.create_room_alias_association",),
        "Written along with room_aliases."),
    "room_aliases": migrated(
        "rooms", ("roomserver_room_aliases",), ("DirectoryStore.create_room_alias_association",),
        "Local room aliases."),
    "room_ban_redactions": derived(_PERSIST, "Only for bans that ask for redaction (MSC4293)."),
    "room_depth": derived(_PERSIST, _PERSISTED),
    "room_forgetter_stream_pos": synapse("Singleton stream position."),
    "room_memberships": derived(
        _PERSIST + ("RoomMemberWorkerStore.forget",),
        "Membership events, plus the forgotten flag from Dendrite's roomserver_membership."),
    "room_reports": empty("Abuse reports aren't carried over."),
    "room_retention": derived(_PERSIST, "From m.room.retention state events."),
    "room_stats_current": derived(_STATS, "Built from the migrated rooms and users."),
    "room_stats_earliest_token": derived(_STATS, "Built from the migrated rooms and users."),
    "room_stats_state": derived(_STATS, "Built from the migrated rooms and users."),
    "room_tags": migrated(
        "account_data", ("userapi_account_datas",), ("TagsWorkerStore.add_tag_to_room",),
        "From m.tag room account data."),
    "room_tags_revisions": derived(("TagsWorkerStore.add_tag_to_room",), "Written with each tag."),
    "rooms": migrated(
        "rooms", ("roomserver_rooms", "roomserver_published"), ("RoomStore.store_room",),
        "Room version, creator and whether the room is published in the directory."),
    "scheduled_tasks": synapse("Synapse's task scheduler."),
    "schema_compat_version": synapse("Schema bookkeeping."),
    "schema_version": synapse("Schema bookkeeping."),
    "server_keys_json": empty("Cache of other servers' signing keys, fetched again when needed."),
    "server_signature_keys": empty("Cache of other servers' signing keys, fetched again when needed."),
    "sessions": empty(_TRANSIENT),
    "sliding_sync_connection_lazy_members": empty(_SLIDING_SYNC_CONN),
    "sliding_sync_connection_positions": empty(_SLIDING_SYNC_CONN),
    "sliding_sync_connection_required_state": empty(_SLIDING_SYNC_CONN),
    "sliding_sync_connection_room_configs": empty(_SLIDING_SYNC_CONN),
    "sliding_sync_connection_streams": empty(_SLIDING_SYNC_CONN),
    "sliding_sync_connections": empty(_SLIDING_SYNC_CONN),
    "sliding_sync_joined_rooms": derived(_PERSIST, _PERSISTED),
    "sliding_sync_joined_rooms_to_recalculate": synapse("Work queue for a background update."),
    "sliding_sync_membership_snapshots": derived(_PERSIST + ("RoomMemberWorkerStore.forget",), _PERSISTED),
    "state_events": derived(_PERSIST, _PERSISTED),
    "state_group_edges": derived(_STATE_GROUPS, "Links each state group to the one it's a delta of."),
    "state_groups": derived(
        _STATE_GROUPS,
        "Created as events are persisted, from the state of their prev events, like for new events. "
        "den2syn checks that the state before every event equals Dendrite's snapshot."),
    "state_groups_pending_deletion": synapse("Work queue for state group garbage collection."),
    "state_groups_persisting": synapse("Locks for state groups being persisted."),
    "state_groups_state": derived(_STATE_GROUPS, "Contents of the state groups, mostly as deltas."),
    "stats_incremental_position": synapse("Singleton stream position; moved forward by the stats background update."),
    "sticky_events": empty("Dendrite has no sticky events (MSC4354)."),
    "stream_ordering_to_exterm": derived(_PERSIST, _PERSISTED),
    "stream_positions": synapse("Positions of multi-writer streams."),
    "thread_subscriptions": empty("Dendrite has no thread subscriptions (MSC4306)."),
    "threads": derived(_PERSIST, _PERSISTED),
    "threepid_guest_access_tokens": empty(_TRANSIENT),
    "threepid_validation_session": empty(_TRANSIENT),
    "threepid_validation_token": empty(_TRANSIENT),
    "timeline_gaps": derived(_PERSIST, "Records where the room history has a gap, like event_backward_extremities."),
    "ui_auth_sessions": empty(_TRANSIENT),
    "ui_auth_sessions_credentials": empty(_TRANSIENT),
    "ui_auth_sessions_ips": empty(_TRANSIENT),
    "un_partial_stated_event_stream": empty("Dendrite has full state for every room it knows."),
    "un_partial_stated_room_stream": empty("Dendrite has full state for every room it knows."),
    "user_daily_visits": empty("Only used for usage statistics."),
    "user_directory": derived(_USER_DIR, "Built from the migrated rooms and users."),
    "user_directory_search": derived(_USER_DIR, "Built from the migrated rooms and users."),
    "user_directory_stale_remote_users": derived(_USER_DIR, "Refreshed as remote profiles change."),
    "user_directory_stream_pos": synapse("Singleton stream position; moved forward by the user directory background update."),
    "user_external_ids": empty("SSO identities; Dendrite only supports password login."),
    "user_filters": migrated(
        "account_data", ("syncapi_filter",), ("DatabasePool.simple_insert",),
        "Inserted directly, because add_user_filter assigns new filter IDs and clients reuse the IDs they have cached."),
    "user_ips": migrated(
        "users", ("userapi_devices",), ("DatabasePool.simple_upsert",),
        "Last seen IP and user agent per device. Synapse's own writer batches live requests."),
    "user_reports": empty("Abuse reports aren't carried over."),
    "user_signature_stream": derived(
        ("E2eKeysHandler.upload_signing_keys_for_user", "E2eKeysHandler.upload_signatures_for_device_keys"),
        "Written as cross-signing keys and signatures are uploaded."),
    "user_stats_current": derived(_STATS, "Built from the migrated rooms and users."),
    "user_threepid_id_server": empty("Dendrite doesn't record identity server bindings."),
    "user_threepids": migrated(
        "users", ("userapi_threepids",), ("RegistrationWorkerStore.user_add_threepid",),
        "Email addresses and phone numbers bound to accounts."),
    "users": migrated(
        "users", ("userapi_accounts",),
        ("RegistrationStore.register_user", "RegistrationWorkerStore.set_user_deactivated_status",
         "DatabasePool.simple_update_one"),
        "Password hash (both use bcrypt), admin and guest flags, and deactivation. creation_ts is "
        "updated directly, because register_user stamps the current time."),
    "users_in_public_rooms": derived(_USER_DIR, "Built from the migrated rooms and users."),
    "users_pending_deactivation": empty("Dendrite completes deactivation immediately."),
    "users_to_send_full_presence_to": empty("Presence is ephemeral."),
    "users_who_share_private_rooms": derived(_USER_DIR, "Built from the migrated rooms and users."),
    "worker_locks": synapse("Runtime locks."),
    "worker_read_write_locks": synapse("Runtime locks."),
    "worker_read_write_locks_mode": synapse("Runtime locks."),
}


class StoryError(Exception):
    pass


def check_tables(tables: set[str]) -> None:
    """Fails if the tables in Synapse's database don't match the stories exactly."""

    missing = sorted(tables - STORIES.keys())
    stale = sorted(STORIES.keys() - tables)
    problems = []
    if missing:
        problems.append("tables without a story: " + ", ".join(missing))
    if stale:
        problems.append("stories for tables that don't exist: " + ", ".join(stale))
    if problems:
        raise StoryError("; ".join(problems))
