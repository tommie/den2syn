package main

import (
	"database/sql"
	"fmt"
)

func (m *Migrator) getRoomMigrationSteps() []MigrationStep {
	return []MigrationStep{
		{
			Name:        "migrate_rooms",
			Description: "Migrate rooms",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				if m.Verbose {
					m.Log.Debug("Migrating rooms from Dendrite to Synapse")
				}

				// Read room data from Dendrite
				rows, err := m.DendriteDB.Query(`
					SELECT
						r.room_id,
						false as is_public,
						'' as creator, -- Will be updated from create events
						COALESCE(r.room_version, '1') as room_version,
						false as has_auth_chain_index
					FROM roomserver_rooms r
					ORDER BY r.room_nid
				`)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite rooms: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse
				stmt, err := tx.Prepare(`
					INSERT INTO rooms (
						room_id,
						is_public,
						creator,
						room_version,
						has_auth_chain_index
					)
					VALUES ($1, $2, $3, $4, $5)
					ON CONFLICT (room_id) DO UPDATE SET
						room_version = EXCLUDED.room_version
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare room insert statement: %w", err)
				}
				defer stmt.Close()

				// Process each room
				count := 0
				for rows.Next() {
					var roomID, creator, roomVersion string
					var isPublic, hasAuthChainIndex bool

					if err := rows.Scan(&roomID, &isPublic, &creator, &roomVersion, &hasAuthChainIndex); err != nil {
						return fmt.Errorf("failed to scan room row: %w", err)
					}

					// Convert booleans to integers for Synapse schema
					isPublicInt := 0
					if isPublic {
						isPublicInt = 1
					}
					hasAuthChainIndexInt := 0
					if hasAuthChainIndex {
						hasAuthChainIndexInt = 1
					}

					if _, err := stmt.Exec(roomID, isPublicInt, creator, roomVersion, hasAuthChainIndexInt); err != nil {
						return fmt.Errorf("failed to insert room: %w", err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading room rows: %w", err)
				}

				m.Log.Debugf("Migrated %d rooms", count)
				return nil
			},
		},
		{
			Name:        "migrate_events",
			Description: "Migrate events",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				if m.Verbose {
					m.Log.Debug("Migrating events from Dendrite to Synapse")
				}

				// This is a complex migration that requires the NID mapping tables to be populated first
				// Read event data from Dendrite with all necessary joins
				rows, err := m.DendriteDB.Query(`
					SELECT
						e.event_id,
						etypes.event_type as type,
						rooms.room_id,
						CASE
							WHEN ej.event_json IS NOT NULL AND ej.event_json != '' THEN
								COALESCE(ej.event_json::jsonb->>'content', '{}')::jsonb
							ELSE '{}'::jsonb
						END as content,
						e.depth,
						ROW_NUMBER() OVER (ORDER BY e.event_nid) as stream_ordering,
						(EXTRACT(EPOCH FROM NOW()) * 1000)::bigint as received_ts,
						CASE
							WHEN ej.event_json IS NOT NULL AND ej.event_json != '' THEN
								CASE
									WHEN (ej.event_json::jsonb->>'sender') LIKE '@%' THEN ej.event_json::jsonb->>'sender'
									WHEN (ej.event_json::jsonb->>'sender') IS NOT NULL THEN '@' || (ej.event_json::jsonb->>'sender')
									ELSE ''
								END
							ELSE ''
						END as sender,
						CASE
							WHEN ej.event_json IS NOT NULL AND ej.event_json != '' THEN
								(ej.event_json LIKE '%"url"%')
							ELSE false
						END as contains_url,
						CASE
							WHEN skeys.event_state_key = '' THEN ''
							WHEN skeys.event_state_key IS NULL THEN NULL
							WHEN etypes.event_type = 'm.room.member' AND skeys.event_state_key NOT LIKE '@%' THEN '@' || skeys.event_state_key
							ELSE skeys.event_state_key
						END as state_key,
						CASE WHEN e.is_rejected THEN 'unknown' ELSE NULL END as rejection_reason,
						CASE
							WHEN ej.event_json IS NOT NULL AND ej.event_json != '' THEN
								COALESCE((ej.event_json::jsonb->>'origin_server_ts')::bigint,
										(EXTRACT(EPOCH FROM NOW()) * 1000)::bigint)
							ELSE (EXTRACT(EPOCH FROM NOW()) * 1000)::bigint
						END as origin_server_ts
					FROM roomserver_events e
					JOIN roomserver_rooms rooms ON e.room_nid = rooms.room_nid
					JOIN roomserver_event_types etypes ON e.event_type_nid = etypes.event_type_nid
					LEFT JOIN roomserver_event_state_keys skeys ON e.event_state_key_nid = skeys.event_state_key_nid
					LEFT JOIN roomserver_event_json ej ON e.event_nid = ej.event_nid
					ORDER BY e.event_nid
				`)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite events: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse
				stmt, err := tx.Prepare(`
					INSERT INTO events (
						topological_ordering,
						event_id,
						type,
						room_id,
						content,
						unrecognized_keys,
						processed,
						outlier,
						depth,
						stream_ordering,
						received_ts,
						sender,
						contains_url,
						state_key,
						rejection_reason,
						instance_name,
						origin_server_ts
					)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, nextval('events_stream_seq'), $10, $11, $12, $13, $14, $15, $16)
					ON CONFLICT (event_id) DO UPDATE SET
						processed = EXCLUDED.processed,
						outlier = EXCLUDED.outlier
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare event insert statement: %w", err)
				}
				defer stmt.Close()

				// Process each event
				count := 0
				for rows.Next() {
					var eventID, eventType, roomID, content, sender string
					var depth, streamOrdering, receivedTs, originServerTs int64
					var stateKey, rejectionReason sql.NullString // Handle nullable columns
					var containsURL bool

					if err := rows.Scan(&eventID, &eventType, &roomID, &content, &depth, &streamOrdering, &receivedTs, &sender, &containsURL, &stateKey, &rejectionReason, &originServerTs); err != nil {
						return fmt.Errorf("failed to scan event row: %w", err)
					}

					// Use stream ordering as topological ordering for simplicity
					topologicalOrdering := streamOrdering
					unrecognizedKeys := sql.NullString{} // NULL
					instanceName := "master"

					// Convert booleans to integers for Synapse schema
					processedInt := 1 // true
					outlierInt := 0   // false
					containsURLInt := 0
					if containsURL {
						containsURLInt = 1
					}

					var stateKeyValue *string
					if stateKey.Valid && stateKey.String != "" {
						stateKeyValue = &stateKey.String
					}
					var rejectionReasonValue *string
					if rejectionReason.Valid && rejectionReason.String != "" {
						rejectionReasonValue = &rejectionReason.String
					}

					if _, err := stmt.Exec(topologicalOrdering, eventID, eventType, roomID, content, unrecognizedKeys, processedInt, outlierInt, depth, receivedTs, sender, containsURLInt, stateKeyValue, rejectionReasonValue, instanceName, originServerTs); err != nil {
						return fmt.Errorf("failed to insert event: %w", err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading event rows: %w", err)
				}

				m.Log.Debugf("Migrated %d events", count)
				return nil
			},
		},
		{
			Name:        "migrate_event_json",
			Description: "Migrate event JSON",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				if m.Verbose {
					m.Log.Debug("Migrating event JSON from Dendrite to Synapse")
				}

				// Read event JSON data from Dendrite with room version to determine format_version
				rows, err := m.DendriteDB.Query(`
					SELECT
						e.event_id,
						rooms.room_id,
						'{}' as internal_metadata,
						ej.event_json as json,
						COALESCE(rooms.room_version, '1') as room_version
					FROM roomserver_events e
					JOIN roomserver_rooms rooms ON e.room_nid = rooms.room_nid
					JOIN roomserver_event_json ej ON e.event_nid = ej.event_nid
					ORDER BY e.event_nid
				`)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite event JSON: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse
				stmt, err := tx.Prepare(`
					INSERT INTO event_json (
						event_id,
						room_id,
						internal_metadata,
						json,
						format_version
					)
					VALUES ($1, $2, $3, $4, $5)
					ON CONFLICT (event_id) DO UPDATE SET
						json = EXCLUDED.json,
						format_version = EXCLUDED.format_version
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare event JSON insert statement: %w", err)
				}
				defer stmt.Close()

				// Process each event JSON
				count := 0
				for rows.Next() {
					var eventID, roomID, internalMetadata, eventJSON, roomVersion string

					if err := rows.Scan(&eventID, &roomID, &internalMetadata, &eventJSON, &roomVersion); err != nil {
						return fmt.Errorf("failed to scan event JSON row: %w", err)
					}

					// Determine format_version based on room version
					formatVersion := m.getEventFormatVersion(roomVersion)

					if _, err := stmt.Exec(eventID, roomID, internalMetadata, eventJSON, formatVersion); err != nil {
						return fmt.Errorf("failed to insert event JSON: %w", err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading event JSON rows: %w", err)
				}

				m.Log.Debugf("Migrated %d event JSON records", count)
				return nil
			},
		},
		{
			Name:        "migrate_room_memberships",
			Description: "Migrate room memberships",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				if m.Verbose {
					m.Log.Debug("Migrating room memberships from Dendrite to Synapse")
				}

				rows, err := m.DendriteDB.Query(`
					SELECT
						e.event_id,
						CASE
							WHEN skeys.event_state_key LIKE '@%' THEN skeys.event_state_key
							WHEN skeys.event_state_key IS NOT NULL THEN '@' || skeys.event_state_key
							ELSE skeys.event_state_key
						END as user_id,
						CASE
							WHEN ej.event_json IS NOT NULL AND ej.event_json != '' THEN
								CASE
									WHEN (ej.event_json::jsonb->>'sender') LIKE '@%' THEN ej.event_json::jsonb->>'sender'
									WHEN (ej.event_json::jsonb->>'sender') IS NOT NULL THEN '@' || (ej.event_json::jsonb->>'sender')
									ELSE ''
								END
							ELSE ''
						END as sender,
						rooms.room_id,
						CASE
							WHEN ej.event_json IS NOT NULL AND ej.event_json != '' THEN
								COALESCE(ej.event_json::jsonb->'content'->>'membership', 'leave')
							ELSE 'leave'
						END as membership,
						CASE
							WHEN ej.event_json IS NOT NULL AND ej.event_json != '' THEN
								ej.event_json::jsonb->'content'->>'display_name'
							ELSE NULL
						END as display_name,
						CASE
							WHEN ej.event_json IS NOT NULL AND ej.event_json != '' THEN
								ej.event_json::jsonb->'content'->>'avatar_url'
							ELSE NULL
						END as avatar_url,
						false as forgotten
					FROM roomserver_events e
					JOIN roomserver_rooms rooms ON e.room_nid = rooms.room_nid
					JOIN roomserver_event_types etypes ON e.event_type_nid = etypes.event_type_nid
					JOIN roomserver_event_state_keys skeys ON e.event_state_key_nid = skeys.event_state_key_nid
					JOIN roomserver_event_json ej ON e.event_nid = ej.event_nid
					WHERE etypes.event_type = 'm.room.member'
					ORDER BY e.event_nid
				`)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite room memberships: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse
				stmt, err := tx.Prepare(`
					INSERT INTO room_memberships (
						event_id,
						user_id,
						sender,
						room_id,
						membership,
						display_name,
						avatar_url,
						forgotten
					)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
					ON CONFLICT (event_id) DO UPDATE SET
						membership = EXCLUDED.membership,
						display_name = EXCLUDED.display_name,
						avatar_url = EXCLUDED.avatar_url
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare room membership insert statement: %w", err)
				}
				defer stmt.Close()

				// Process each room membership
				count := 0
				for rows.Next() {
					var eventID, userID, sender, roomID, membership string
					var displayName, avatarURL sql.NullString
					var forgotten bool

					if err := rows.Scan(&eventID, &userID, &sender, &roomID, &membership, &displayName, &avatarURL, &forgotten); err != nil {
						return fmt.Errorf("failed to scan room membership row: %w", err)
					}

					var displayNameValue, avatarURLValue *string
					if displayName.Valid {
						displayNameValue = &displayName.String
					}
					if avatarURL.Valid {
						avatarURLValue = &avatarURL.String
					}

					// Convert boolean to integer for Synapse schema
					forgottenInt := 0
					if forgotten {
						forgottenInt = 1
					}

					if _, err := stmt.Exec(eventID, userID, sender, roomID, membership, displayNameValue, avatarURLValue, forgottenInt); err != nil {
						return fmt.Errorf("failed to insert room membership: %w", err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading room membership rows: %w", err)
				}

				m.Log.Debugf("Migrated %d room memberships", count)
				return nil
			},
		},
		{
			Name:        "migrate_local_current_membership",
			Description: "Migrate local current membership for room visibility",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				if m.Verbose {
					m.Log.Debug("Migrating local current membership from Dendrite to Synapse")
				}

				// Query to get the latest membership state for each user in each room
				rows, err := m.DendriteDB.Query(`
					SELECT DISTINCT ON (rooms.room_id, skeys.event_state_key)
						e.event_id,
						rooms.room_id,
						CASE
							WHEN skeys.event_state_key LIKE '@%' THEN skeys.event_state_key
							WHEN skeys.event_state_key IS NOT NULL THEN '@' || skeys.event_state_key
							ELSE skeys.event_state_key
						END as user_id,
						CASE
							WHEN ej.event_json IS NOT NULL AND ej.event_json != '' THEN
								COALESCE(ej.event_json::jsonb->'content'->>'membership', 'leave')
							ELSE 'leave'
						END as membership
					FROM roomserver_events e
					JOIN roomserver_rooms rooms ON e.room_nid = rooms.room_nid
					JOIN roomserver_event_types etypes ON e.event_type_nid = etypes.event_type_nid
					JOIN roomserver_event_state_keys skeys ON e.event_state_key_nid = skeys.event_state_key_nid
					LEFT JOIN roomserver_event_json ej ON e.event_nid = ej.event_nid
					WHERE etypes.event_type = 'm.room.member'
					  AND skeys.event_state_key IS NOT NULL
					  AND skeys.event_state_key LIKE '%:' || $1  -- Only local users
					ORDER BY rooms.room_id, skeys.event_state_key, e.event_nid DESC
				`, m.ServerName)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite local current membership: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse
				stmt, err := tx.Prepare(`
					INSERT INTO local_current_membership (
						room_id,
						user_id,
						event_id,
						membership
					)
					VALUES ($1, $2, $3, $4)
					ON CONFLICT (room_id, user_id) DO UPDATE SET
						event_id = EXCLUDED.event_id,
						membership = EXCLUDED.membership
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare local current membership insert statement: %w", err)
				}
				defer stmt.Close()

				// Process each membership record
				count := 0
				for rows.Next() {
					var eventID, roomID, userID, membership string

					if err := rows.Scan(&eventID, &roomID, &userID, &membership); err != nil {
						return fmt.Errorf("failed to scan local current membership row: %w", err)
					}

					if _, err := stmt.Exec(roomID, userID, eventID, membership); err != nil {
						return fmt.Errorf("failed to insert local current membership: %w", err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading local current membership rows: %w", err)
				}

				m.Log.Debugf("Migrated %d local current membership records", count)
				return nil
			},
		},
	}
}

// getEventFormatVersion determines the event format version based on room version
// According to Synapse's EventFormatVersions:
// - ROOM_V1_V2 = 1 (for room v1 and v2)
// - ROOM_V3 = 2 (for room v3)
// - ROOM_V4_PLUS = 3 (for room v4 and later)
func (m *Migrator) getEventFormatVersion(roomVersion string) int {
	switch roomVersion {
	case "1", "2":
		return 1 // EventFormatVersions.ROOM_V1_V2
	case "3":
		return 2 // EventFormatVersions.ROOM_V3
	case "4", "5", "6", "7", "8", "9", "10", "11", "12", "13", "14", "15":
		return 3 // EventFormatVersions.ROOM_V4_PLUS
	default:
		// For any unknown or newer room version, default to the latest format
		m.Log.Warnf("Unknown room version '%s', defaulting to format version 3", roomVersion)
		return 3 // EventFormatVersions.ROOM_V4_PLUS
	}
}
