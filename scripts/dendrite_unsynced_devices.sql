-- Lists Dendrite devices that may not have synced every event before cutover.
--
-- Clients keep their Dendrite sync token, and Synapse lowers its room
-- position to Synapse's current one, so a device silently skips events it
-- hadn't synced. Its user then has to clear its cache.
--
-- Dendrite doesn't store sync positions, but sets a device's last_seen_ts when
-- a sync request starts, and a request that started after the newest event
-- the user could see returned everything. So a device last seen after that
-- event has synced. Dendrite updates last_seen_ts at most once a minute, so a
-- device may be listed even if it has synced, but not the other way around,
-- except if it lost the response, or a remote server's clock was ahead.
--
-- Run after stopping Dendrite: psql -f dendrite_unsynced_devices.sql dendrite

WITH memberships AS (
    SELECT DISTINCT ON (room_id, user_id) room_id, user_id, membership, stream_pos
    FROM syncapi_memberships
    ORDER BY room_id, user_id, stream_pos DESC
), newest AS (
    -- The newest event each user could sync: anything in rooms they're in,
    -- and up to their leave in rooms they left.
    SELECT m.user_id, MAX((e.headered_event_json::jsonb->>'origin_server_ts')::bigint) AS ts
    FROM memberships m
    JOIN syncapi_output_room_events e ON e.room_id = m.room_id
    WHERE m.membership IN ('join', 'invite') OR e.id <= m.stream_pos
    GROUP BY m.user_id
)
SELECT
    '@' || d.localpart || ':' || d.server_name AS user_id,
    d.device_id,
    d.display_name,
    to_timestamp(d.last_seen_ts / 1000.0)::timestamp(0) AS last_seen,
    to_timestamp(n.ts / 1000.0)::timestamp(0) AS newest_event
FROM userapi_devices d
JOIN newest n ON n.user_id = '@' || d.localpart || ':' || d.server_name
WHERE d.last_seen_ts <= n.ts
ORDER BY d.last_seen_ts DESC;
