package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// No imports needed for now

func (m *Migrator) getMigrationPhases() []MigrationPhase {
	return []MigrationPhase{
		{
			Name:        "Phase 1",
			Description: "Preparation and Validation",
			Steps:       m.getPreparationSteps(),
		},
		{
			Name:        "Phase 2",
			Description: "NID Resolution System",
			Steps:       m.getNIDResolutionSteps(),
		},
		{
			Name:        "Phase 3",
			Description: "Core Data Migration",
			Steps:       m.getCoreDataMigrationSteps(),
		},
		{
			Name:        "Phase 4",
			Description: "State and Membership Migration",
			Steps:       m.getStateMigrationSteps(),
		},
		{
			Name:        "Phase 5",
			Description: "Advanced Features",
			Steps:       m.getAdvancedFeaturesSteps(),
		},
	}
}

func (m *Migrator) getPreparationSteps() []MigrationStep {
	return []MigrationStep{
		{
			Name:        "list_all_tables",
			Description: "List all available tables for debugging",
			Run: m.newDendriteQuery(`
				SELECT
					schemaname,
					tablename
				FROM pg_tables
				ORDER BY schemaname, tablename;
			`),
		},
		{
			Name:        "check_required_tables",
			Description: "Check for required Dendrite tables",
			Run: m.newDendriteQuery(`
				SELECT
					tablename as found_table,
					'Table exists: ' || tablename as status
				FROM pg_tables
				WHERE tablename IN (
					'roomserver_events', 'roomserver_event_json', 'roomserver_rooms',
					'userapi_accounts', 'userapi_devices', 'syncapi_output_room_events'
				)
				ORDER BY tablename;
			`),
		},
		{
			Name:        "analyze_event_coverage",
			Description: "Check event coverage and missing event_json",
			Run: m.newDendriteQuery(`
				SELECT
					'Total events' as metric,
					COUNT(*) as count
				FROM roomserver_events
				UNION ALL
				SELECT
					'Events with rooms' as metric,
					COUNT(*)
				FROM roomserver_events e
				JOIN roomserver_rooms rooms ON e.room_nid = rooms.room_nid
				UNION ALL
				SELECT
					'Events with types' as metric,
					COUNT(*)
				FROM roomserver_events e
				JOIN roomserver_event_types etypes ON e.event_type_nid = etypes.event_type_nid
				UNION ALL
				SELECT
					'Events with JSON' as metric,
					COUNT(*)
				FROM roomserver_events e
				JOIN roomserver_event_json ej ON e.event_nid = ej.event_nid
				UNION ALL
				SELECT
					'Events with all joins' as metric,
					COUNT(*)
				FROM roomserver_events e
				JOIN roomserver_rooms rooms ON e.room_nid = rooms.room_nid
				JOIN roomserver_event_types etypes ON e.event_type_nid = etypes.event_type_nid
				LEFT JOIN roomserver_event_state_keys skeys ON e.event_state_key_nid = skeys.event_state_key_nid
				LEFT JOIN roomserver_event_json ej ON e.event_nid = ej.event_nid;
			`),
		},
		{
			Name:        "validate_table_count",
			Description: "Validate all required tables are present",
			Run: m.newDendriteQuery(`
				WITH required_count AS (
					SELECT COUNT(*) as found_tables
					FROM pg_tables
					WHERE tablename IN (
						'roomserver_events', 'roomserver_event_json', 'roomserver_rooms',
						'userapi_accounts', 'userapi_devices', 'syncapi_output_room_events'
					)
				)
				SELECT
					found_tables,
					CASE
						WHEN found_tables = 6 THEN 'SUCCESS: All 6 required tables found'
						ELSE 'ERROR: Only ' || found_tables || ' of 6 required tables found'
					END as validation_result
				FROM required_count;
			`),
		},
		{
			Name:        "integrity_check",
			Description: "Check data integrity",
			Run: m.newDendriteQuery(`
				SELECT
					COUNT(*) as orphaned_events,
					'Orphaned events (events without JSON)' as description
				FROM roomserver_events e
				LEFT JOIN roomserver_event_json ej ON e.event_nid = ej.event_nid
				WHERE ej.event_nid IS NULL;
			`),
		},
	}
}

func (m *Migrator) getNIDResolutionSteps() []MigrationStep {
	return []MigrationStep{
		{
			Name:        "create_mapping_tables",
			Description: "Create NID mapping tables",
			Run: m.newSynapseCommand(`
				-- Create temporary mapping tables for NID resolution
				CREATE TEMP TABLE IF NOT EXISTS nid_room_mapping (
					room_nid BIGINT PRIMARY KEY,
					room_id TEXT NOT NULL
				);

				CREATE TEMP TABLE IF NOT EXISTS nid_event_mapping (
					event_nid BIGINT PRIMARY KEY,
					event_id TEXT NOT NULL
				);

				CREATE TEMP TABLE IF NOT EXISTS nid_eventtype_mapping (
					event_type_nid BIGINT PRIMARY KEY,
					event_type TEXT NOT NULL
				);

				CREATE TEMP TABLE IF NOT EXISTS nid_statekey_mapping (
					event_state_key_nid BIGINT PRIMARY KEY,
					event_state_key TEXT
				);
			`),
		},
		{
			Name:        "populate_room_mapping",
			Description: "Populate room NID mappings",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				if m.Verbose {
					m.Log.Debug("Populating room NID mapping from Dendrite to Synapse")
				}

				// Read room data from Dendrite
				rows, err := m.DendriteDB.Query(`
					SELECT room_nid, room_id
					FROM roomserver_rooms
					ORDER BY room_nid
				`)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite rooms: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse
				stmt, err := tx.Prepare(`
					INSERT INTO nid_room_mapping (room_nid, room_id)
					VALUES ($1, $2)
					ON CONFLICT (room_nid) DO UPDATE SET room_id = EXCLUDED.room_id
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare room mapping statement: %w", err)
				}
				defer stmt.Close()

				// Process each room
				count := 0
				for rows.Next() {
					var roomNID int64
					var roomID string

					if err := rows.Scan(&roomNID, &roomID); err != nil {
						return fmt.Errorf("failed to scan room row: %w", err)
					}

					if _, err := stmt.Exec(roomNID, roomID); err != nil {
						return fmt.Errorf("failed to insert room mapping: %w", err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading room rows: %w", err)
				}

				m.Log.Debugf("Populated %d room mappings", count)
				return nil
			},
		},
		{
			Name:        "populate_event_mapping",
			Description: "Populate event NID mappings",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				if m.Verbose {
					m.Log.Debug("Populating event NID mapping from Dendrite to Synapse")
				}

				// Read event data from Dendrite
				rows, err := m.DendriteDB.Query(`
					SELECT event_nid, event_id
					FROM roomserver_events
					ORDER BY event_nid
				`)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite events: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse
				stmt, err := tx.Prepare(`
					INSERT INTO nid_event_mapping (event_nid, event_id)
					VALUES ($1, $2)
					ON CONFLICT (event_nid) DO UPDATE SET event_id = EXCLUDED.event_id
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare event mapping statement: %w", err)
				}
				defer stmt.Close()

				// Process each event
				count := 0
				for rows.Next() {
					var eventNID int64
					var eventID string

					if err := rows.Scan(&eventNID, &eventID); err != nil {
						return fmt.Errorf("failed to scan event row: %w", err)
					}

					if _, err := stmt.Exec(eventNID, eventID); err != nil {
						return fmt.Errorf("failed to insert event mapping: %w", err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading event rows: %w", err)
				}

				m.Log.Debugf("Populated %d event mappings", count)
				return nil
			},
		},
		{
			Name:        "populate_eventtype_mapping",
			Description: "Populate event type NID mappings",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				if m.Verbose {
					m.Log.Debug("Populating event type NID mapping from Dendrite to Synapse")
				}

				// Read event type data from Dendrite
				rows, err := m.DendriteDB.Query(`
					SELECT event_type_nid, event_type
					FROM roomserver_event_types
					ORDER BY event_type_nid
				`)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite event types: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse
				stmt, err := tx.Prepare(`
					INSERT INTO nid_eventtype_mapping (event_type_nid, event_type)
					VALUES ($1, $2)
					ON CONFLICT (event_type_nid) DO UPDATE SET event_type = EXCLUDED.event_type
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare event type mapping statement: %w", err)
				}
				defer stmt.Close()

				// Process each event type
				count := 0
				for rows.Next() {
					var eventTypeNID int64
					var eventType string

					if err := rows.Scan(&eventTypeNID, &eventType); err != nil {
						return fmt.Errorf("failed to scan event type row: %w", err)
					}

					if _, err := stmt.Exec(eventTypeNID, eventType); err != nil {
						return fmt.Errorf("failed to insert event type mapping: %w", err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading event type rows: %w", err)
				}

				m.Log.Debugf("Populated %d event type mappings", count)
				return nil
			},
		},
		{
			Name:        "populate_statekey_mapping",
			Description: "Populate state key NID mappings",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				if m.Verbose {
					m.Log.Debug("Populating state key NID mapping from Dendrite to Synapse")
				}

				// Read state key data from Dendrite
				rows, err := m.DendriteDB.Query(`
					SELECT event_state_key_nid, event_state_key
					FROM roomserver_event_state_keys
					ORDER BY event_state_key_nid
				`)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite state keys: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse
				stmt, err := tx.Prepare(`
					INSERT INTO nid_statekey_mapping (event_state_key_nid, event_state_key)
					VALUES ($1, $2)
					ON CONFLICT (event_state_key_nid) DO UPDATE SET event_state_key = EXCLUDED.event_state_key
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare state key mapping statement: %w", err)
				}
				defer stmt.Close()

				// Process each state key
				count := 0
				for rows.Next() {
					var stateKeyNID int64
					var stateKey sql.NullString

					if err := rows.Scan(&stateKeyNID, &stateKey); err != nil {
						return fmt.Errorf("failed to scan state key row: %w", err)
					}

					var stateKeyValue *string
					if stateKey.Valid {
						stateKeyValue = &stateKey.String
					}

					if _, err := stmt.Exec(stateKeyNID, stateKeyValue); err != nil {
						return fmt.Errorf("failed to insert state key mapping: %w", err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading state key rows: %w", err)
				}

				m.Log.Debugf("Populated %d state key mappings", count)
				return nil
			},
		},
	}
}

func (m *Migrator) getCoreDataMigrationSteps() []MigrationStep {
	return []MigrationStep{
		{
			Name:        "migrate_users",
			Description: "Migrate user accounts",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				if m.Verbose {
					m.Log.Debug("Migrating users from Dendrite to Synapse")
				}

				// Read user data from Dendrite
				rows, err := m.DendriteDB.Query(`
					SELECT
						'@' || localpart || ':' || server_name as name,
						password_hash,
						created_ts,
						CASE WHEN account_type = 1 THEN true ELSE false END as admin,
						CASE WHEN account_type = 2 THEN true ELSE false END as is_guest,
						COALESCE(is_deactivated, false) as deactivated,
						false as shadow_banned,
						COALESCE(account_type, 0) as user_type,
						appservice_id
					FROM userapi_accounts
					ORDER BY created_ts
				`)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite users: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse
				stmt, err := tx.Prepare(`
					INSERT INTO users (
						name,
						password_hash,
						creation_ts,
						admin,
						is_guest,
						deactivated,
						shadow_banned,
						user_type,
						appservice_id
					)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
					ON CONFLICT (name) DO UPDATE SET
						password_hash = EXCLUDED.password_hash,
						creation_ts = EXCLUDED.creation_ts,
						admin = EXCLUDED.admin,
						is_guest = EXCLUDED.is_guest,
						deactivated = EXCLUDED.deactivated,
						user_type = EXCLUDED.user_type,
						appservice_id = EXCLUDED.appservice_id
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare user insert statement: %w", err)
				}
				defer stmt.Close()

				// Process each user
				count := 0
				for rows.Next() {
					var name, passwordHash string
					var creationTs int64
					var admin, isGuest, deactivated, shadowBanned bool
					var userType int
					var appserviceID sql.NullString

					if err := rows.Scan(&name, &passwordHash, &creationTs, &admin, &isGuest, &deactivated, &shadowBanned, &userType, &appserviceID); err != nil {
						return fmt.Errorf("failed to scan user row: %w", err)
					}

					var appserviceIDValue *string
					if appserviceID.Valid {
						appserviceIDValue = &appserviceID.String
					}

					// Convert booleans to integers for Synapse schema (0/1 instead of true/false)
					adminInt := 0
					if admin {
						adminInt = 1
					}
					isGuestInt := 0
					if isGuest {
						isGuestInt = 1
					}
					deactivatedInt := 0
					if deactivated {
						deactivatedInt = 1
					}
					shadowBannedInt := 0
					if shadowBanned {
						shadowBannedInt = 1
					}

					if _, err := stmt.Exec(name, passwordHash, creationTs, adminInt, isGuestInt, deactivatedInt, shadowBannedInt, userType, appserviceIDValue); err != nil {
						return fmt.Errorf("failed to insert user: %w", err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading user rows: %w", err)
				}

				m.Log.Debugf("Migrated %d users", count)
				return nil
			},
		},
		{
			Name:        "migrate_devices",
			Description: "Migrate user devices",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				if m.Verbose {
					m.Log.Debug("Migrating devices from Dendrite to Synapse")
				}

				// Read device data from Dendrite
				rows, err := m.DendriteDB.Query(`
					SELECT
						'@' || d.localpart || ':' || d.server_name as user_id,
						d.device_id,
						d.display_name,
						d.last_seen_ts,
						d.ip,
						d.user_agent,
						false as hidden
					FROM userapi_devices d
					JOIN userapi_accounts a ON d.localpart = a.localpart AND d.server_name = a.server_name
					ORDER BY d.created_ts
				`)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite devices: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse
				stmt, err := tx.Prepare(`
					INSERT INTO devices (
						user_id,
						device_id,
						display_name,
						last_seen,
						ip,
						user_agent,
						hidden
					)
					VALUES ($1, $2, $3, $4, $5, $6, $7)
					ON CONFLICT (user_id, device_id) DO UPDATE SET
						display_name = EXCLUDED.display_name,
						last_seen = EXCLUDED.last_seen,
						ip = EXCLUDED.ip,
						user_agent = EXCLUDED.user_agent
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare device insert statement: %w", err)
				}
				defer stmt.Close()

				// Process each device
				count := 0
				for rows.Next() {
					var userID, deviceID string
					var displayName sql.NullString
					var lastSeen sql.NullInt64
					var ip, userAgent sql.NullString
					var hidden bool

					if err := rows.Scan(&userID, &deviceID, &displayName, &lastSeen, &ip, &userAgent, &hidden); err != nil {
						return fmt.Errorf("failed to scan device row: %w", err)
					}

					var displayNameValue, ipValue, userAgentValue *string
					var lastSeenValue *int64
					if displayName.Valid {
						displayNameValue = &displayName.String
					}
					if lastSeen.Valid {
						lastSeenValue = &lastSeen.Int64
					}
					if ip.Valid {
						ipValue = &ip.String
					}
					if userAgent.Valid {
						userAgentValue = &userAgent.String
					}

					// Convert boolean to integer for Synapse schema
					hiddenInt := 0
					if hidden {
						hiddenInt = 1
					}

					if _, err := stmt.Exec(userID, deviceID, displayNameValue, lastSeenValue, ipValue, userAgentValue, hiddenInt); err != nil {
						return fmt.Errorf("failed to insert device: %w", err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading device rows: %w", err)
				}

				m.Log.Debugf("Migrated %d devices", count)
				return nil
			},
		},
		{
			Name:        "migrate_access_tokens",
			Description: "Migrate access tokens",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				if m.Verbose {
					m.Log.Debug("Migrating access tokens from Dendrite to Synapse")

					// Debug: Check for sequences related to access_tokens
					rows, err := tx.Query(`
						SELECT sequence_name
						FROM information_schema.sequences
						WHERE sequence_name LIKE '%access_token%'
					`)
					if err == nil {
						m.Log.Debug("access_tokens related sequences:")
						for rows.Next() {
							var seqName string
							if err := rows.Scan(&seqName); err == nil {
								m.Log.Debugf("  sequence: %s", seqName)
							}
						}
						rows.Close()
					}
				}

				// Read access token data from Dendrite
				rows, err := m.DendriteDB.Query(`
					SELECT
						'@' || d.localpart || ':' || d.server_name as user_id,
						d.device_id,
						d.access_token as token,
						NULL as valid_until_ms,
						NULL as puppets_user_id,
						NULL as last_validated,
						NULL as refresh_token_id,
						false as used
					FROM userapi_devices d
					JOIN userapi_accounts a ON d.localpart = a.localpart AND d.server_name = a.server_name
					WHERE d.access_token IS NOT NULL
					ORDER BY d.created_ts
				`)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite access tokens: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse (using row_number for id)
				stmt, err := tx.Prepare(`
					INSERT INTO access_tokens (
						id,
						user_id,
						device_id,
						token,
						valid_until_ms,
						puppets_user_id,
						last_validated,
						refresh_token_id,
						used
					)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
					ON CONFLICT (token) DO NOTHING
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare access token insert statement: %w", err)
				}
				defer stmt.Close()

				// Process each access token
				count := 0
				for rows.Next() {
					var userID, deviceID, token string
					var validUntilMs, puppetsUserID, lastValidated, refreshTokenID sql.NullInt64
					var used bool

					if err := rows.Scan(&userID, &deviceID, &token, &validUntilMs, &puppetsUserID, &lastValidated, &refreshTokenID, &used); err != nil {
						return fmt.Errorf("failed to scan access token row: %w", err)
					}

					var validUntilMsValue, puppetsUserIDValue, lastValidatedValue, refreshTokenIDValue *int64
					if validUntilMs.Valid {
						validUntilMsValue = &validUntilMs.Int64
					}
					if puppetsUserID.Valid {
						puppetsUserIDValue = &puppetsUserID.Int64
					}
					if lastValidated.Valid {
						lastValidatedValue = &lastValidated.Int64
					}
					if refreshTokenID.Valid {
						refreshTokenIDValue = &refreshTokenID.Int64
					}

					// Convert boolean to integer for Synapse schema
					usedInt := 0
					if used {
						usedInt = 1
					}

					// Generate an ID (using count + 1, starting from 1)
					tokenID := int64(count + 1)

					if _, err := stmt.Exec(tokenID, userID, deviceID, token, validUntilMsValue, puppetsUserIDValue, lastValidatedValue, refreshTokenIDValue, usedInt); err != nil {
						return fmt.Errorf("failed to insert access token: %w", err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading access token rows: %w", err)
				}

				m.Log.Debugf("Migrated %d access tokens", count)
				return nil
			},
		},
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

func (m *Migrator) getStateMigrationSteps() []MigrationStep {
	return []MigrationStep{
		{
			Name:        "migrate_current_state",
			Description: "Migrate current room state",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				if m.Verbose {
					m.Log.Debug("Migrating current state from Dendrite to Synapse")
				}

				// This is a simplified version - real implementation would need proper state resolution
				rows, err := m.DendriteDB.Query(`
					SELECT DISTINCT ON (rooms.room_id, etypes.event_type, skeys.event_state_key)
						e.event_id,
						rooms.room_id,
						etypes.event_type as type,
						skeys.event_state_key as state_key,
						CASE
							WHEN etypes.event_type = 'm.room.member' AND ej.event_json IS NOT NULL AND ej.event_json != '' THEN
								COALESCE(ej.event_json::jsonb->'content'->>'membership', 'leave')
							WHEN etypes.event_type = 'm.room.member' THEN 'leave'
							ELSE NULL
						END as membership
					FROM roomserver_events e
					JOIN roomserver_rooms rooms ON e.room_nid = rooms.room_nid
					JOIN roomserver_event_types etypes ON e.event_type_nid = etypes.event_type_nid
					JOIN roomserver_event_state_keys skeys ON e.event_state_key_nid = skeys.event_state_key_nid
					JOIN roomserver_event_json ej ON e.event_nid = ej.event_nid
					WHERE skeys.event_state_key IS NOT NULL  -- Only state events
					ORDER BY rooms.room_id, etypes.event_type, skeys.event_state_key, e.event_nid DESC
				`)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite current state: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse
				stmt, err := tx.Prepare(`
					INSERT INTO current_state_events (
						event_id,
						room_id,
						type,
						state_key,
						membership
					)
					VALUES ($1, $2, $3, $4, $5)
					ON CONFLICT (room_id, type, state_key) DO UPDATE SET
						event_id = EXCLUDED.event_id,
						membership = EXCLUDED.membership
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare current state insert statement: %w", err)
				}
				defer stmt.Close()

				// Process each current state event
				count := 0
				for rows.Next() {
					var eventID, roomID, eventType, stateKey string
					var membership sql.NullString

					if err := rows.Scan(&eventID, &roomID, &eventType, &stateKey, &membership); err != nil {
						return fmt.Errorf("failed to scan current state row: %w", err)
					}

					var membershipValue *string
					if membership.Valid {
						membershipValue = &membership.String
					}

					if _, err := stmt.Exec(eventID, roomID, eventType, stateKey, membershipValue); err != nil {
						return fmt.Errorf("failed to insert current state: %w", err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading current state rows: %w", err)
				}

				m.Log.Debugf("Migrated %d current state events", count)
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
		{
			Name:        "migrate_state_groups",
			Description: "Migrate state groups for proper state resolution",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				if m.Verbose {
					m.Log.Debug("Migrating state groups from Dendrite to Synapse")
				}

				// Create state groups based on Dendrite's state snapshots
				// Each unique state snapshot becomes a state group
				// We need to find a representative event for each state group
				rows, err := m.DendriteDB.Query(`
					SELECT DISTINCT
						r.state_snapshot_nid as state_group_id,
						r.room_id,
						(SELECT e.event_id
						 FROM roomserver_events e
						 WHERE e.room_nid = r.room_nid
						 ORDER BY e.event_nid DESC
						 LIMIT 1) as event_id
					FROM roomserver_rooms r
					WHERE r.state_snapshot_nid IS NOT NULL
					ORDER BY r.state_snapshot_nid
				`)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite state snapshots: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for state groups
				stmt, err := tx.Prepare(`
					INSERT INTO state_groups (id, room_id, event_id)
					VALUES ($1, $2, $3)
					ON CONFLICT (id) DO NOTHING
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare state groups insert: %w", err)
				}
				defer stmt.Close()

				// Insert state groups
				count := 0
				for rows.Next() {
					var stateGroupID int64
					var roomID string
					var eventID string

					if err := rows.Scan(&stateGroupID, &roomID, &eventID); err != nil {
						return fmt.Errorf("failed to scan state group row: %w", err)
					}

					if _, err := stmt.Exec(stateGroupID, roomID, eventID); err != nil {
						return fmt.Errorf("failed to insert state group: %w", err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading state group rows: %w", err)
				}

				m.Log.Debugf("Migrated %d state groups", count)

				// Now populate state_groups_state with the actual state content
				// We'll use the current state events approach since state_block structure may be different
				rows2, err := m.DendriteDB.Query(`
					SELECT DISTINCT ON (r.state_snapshot_nid, et.event_type, COALESCE(esk.event_state_key, ''))
						r.state_snapshot_nid as state_group,
						r.room_id,
						et.event_type as type,
						COALESCE(esk.event_state_key, '') as state_key,
						e.event_id
					FROM roomserver_events e
					JOIN roomserver_rooms r ON e.room_nid = r.room_nid
					JOIN roomserver_event_types et ON e.event_type_nid = et.event_type_nid
					LEFT JOIN roomserver_event_state_keys esk ON e.event_state_key_nid = esk.event_state_key_nid
					WHERE esk.event_state_key_nid IS NOT NULL  -- Only state events
					  AND r.state_snapshot_nid IS NOT NULL
					ORDER BY r.state_snapshot_nid, et.event_type, COALESCE(esk.event_state_key, ''), e.event_nid DESC
				`)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite state blocks: %w", err)
				}
				defer rows2.Close()

				// Prepare insert for state_groups_state
				stmt2, err := tx.Prepare(`
					INSERT INTO state_groups_state (state_group, room_id, type, state_key, event_id)
					VALUES ($1, $2, $3, $4, $5)
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare state_groups_state insert: %w", err)
				}
				defer stmt2.Close()

				// Insert state group contents
				count2 := 0
				for rows2.Next() {
					var stateGroup int64
					var roomID, eventType, stateKey, eventID string

					if err := rows2.Scan(&stateGroup, &roomID, &eventType, &stateKey, &eventID); err != nil {
						return fmt.Errorf("failed to scan state block row: %w", err)
					}

					if _, err := stmt2.Exec(stateGroup, roomID, eventType, stateKey, eventID); err != nil {
						return fmt.Errorf("failed to insert state group state: %w", err)
					}

					count2++
				}

				if err := rows2.Err(); err != nil {
					return fmt.Errorf("error reading state block rows: %w", err)
				}

				m.Log.Debugf("Migrated %d state group state entries", count2)
				return nil
			},
		},
		{
			Name:        "migrate_event_to_state_groups",
			Description: "Map events to state groups",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				if m.Verbose {
					m.Log.Debug("Migrating event to state group mappings from Dendrite to Synapse")
				}

				// Map each event to its state group based on the room's state snapshot
				rows, err := m.DendriteDB.Query(`
					SELECT
						e.event_id,
						r.state_snapshot_nid as state_group
					FROM roomserver_events e
					JOIN roomserver_rooms r ON e.room_nid = r.room_nid
					WHERE r.state_snapshot_nid IS NOT NULL
					ORDER BY e.event_nid
				`)
				if err != nil {
					return fmt.Errorf("failed to query event to state group mappings: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement
				stmt, err := tx.Prepare(`
					INSERT INTO event_to_state_groups (event_id, state_group)
					VALUES ($1, $2)
					ON CONFLICT (event_id) DO NOTHING
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare event_to_state_groups insert: %w", err)
				}
				defer stmt.Close()

				// Insert mappings
				count := 0
				for rows.Next() {
					var eventID string
					var stateGroup int64

					if err := rows.Scan(&eventID, &stateGroup); err != nil {
						return fmt.Errorf("failed to scan event to state group row: %w", err)
					}

					if _, err := stmt.Exec(eventID, stateGroup); err != nil {
						return fmt.Errorf("failed to insert event to state group mapping: %w", err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading event to state group rows: %w", err)
				}

				m.Log.Debugf("Migrated %d event to state group mappings", count)
				return nil
			},
		},
	}
}

func (m *Migrator) getAdvancedFeaturesSteps() []MigrationStep {
	return []MigrationStep{
		{
			Name:        "migrate_profiles",
			Description: "Migrate user profiles (display names and avatars)",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				m.Log.Info("Migrating user profiles...")

				// Query Dendrite for user profiles
				rows, err := m.DendriteDB.Query(`
					SELECT
						localpart,
						server_name,
						display_name,
						avatar_url
					FROM userapi_profiles
					WHERE server_name = $1
					ORDER BY localpart
				`, m.ServerName)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite profiles: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse profiles
				stmt, err := tx.Prepare(`
					INSERT INTO profiles (user_id, displayname, avatar_url, full_user_id)
					VALUES ($1, $2, $3, $4)
					ON CONFLICT (user_id) DO UPDATE SET
						displayname = EXCLUDED.displayname,
						avatar_url = EXCLUDED.avatar_url,
						full_user_id = EXCLUDED.full_user_id
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare profiles insert: %w", err)
				}
				defer stmt.Close()

				count := 0
				for rows.Next() {
					var localpart, serverName string
					var displayName, avatarURL sql.NullString

					if err := rows.Scan(&localpart, &serverName, &displayName, &avatarURL); err != nil {
						return fmt.Errorf("failed to scan profile row: %w", err)
					}

					// Convert to full user ID format
					userID := "@" + localpart + ":" + serverName

					// Convert NULL strings
					var displayNameStr, avatarURLStr *string
					if displayName.Valid {
						displayNameStr = &displayName.String
					}
					if avatarURL.Valid {
						avatarURLStr = &avatarURL.String
					}

					if _, err := stmt.Exec(userID, displayNameStr, avatarURLStr, userID); err != nil {
						return fmt.Errorf("failed to insert profile for user %s: %w", userID, err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading profile rows: %w", err)
				}

				m.Log.Debugf("Migrated %d user profiles", count)
				return nil
			},
		},
		{
			Name:        "migrate_account_data",
			Description: "Migrate global account data",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				m.Log.Info("Migrating global account data...")

				// Query Dendrite for global account data (room_id is empty string)
				rows, err := m.DendriteDB.Query(`
					SELECT
						localpart,
						server_name,
						type,
						content
					FROM userapi_account_datas
					WHERE server_name = $1 AND (room_id = '' OR room_id IS NULL)
					ORDER BY localpart, type
				`, m.ServerName)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite global account data: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse account_data
				stmt, err := tx.Prepare(`
					INSERT INTO account_data (user_id, account_data_type, stream_id, content, instance_name)
					VALUES ($1, $2, nextval('account_data_sequence'), $3, $4)
					ON CONFLICT (user_id, account_data_type) DO UPDATE SET
						content = EXCLUDED.content,
						stream_id = EXCLUDED.stream_id
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare account_data insert: %w", err)
				}
				defer stmt.Close()

				count := 0
				for rows.Next() {
					var localpart, serverName, dataType, content string

					if err := rows.Scan(&localpart, &serverName, &dataType, &content); err != nil {
						return fmt.Errorf("failed to scan account data row: %w", err)
					}

					// Skip push rules - they'll be handled separately
					if dataType == "m.push_rules" {
						continue
					}

					// Convert to full user ID format
					userID := "@" + localpart + ":" + serverName

					// Use 'master' as default instance name
					if _, err := stmt.Exec(userID, dataType, content, "master"); err != nil {
						return fmt.Errorf("failed to insert account data for user %s type %s: %w", userID, dataType, err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading account data rows: %w", err)
				}

				m.Log.Debugf("Migrated %d global account data entries", count)
				return nil
			},
		},
		{
			Name:        "migrate_room_account_data",
			Description: "Migrate room-specific account data",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				m.Log.Info("Migrating room account data...")

				// Query Dendrite for room-specific account data (room_id is not empty)
				rows, err := m.DendriteDB.Query(`
					SELECT
						localpart,
						server_name,
						room_id,
						type,
						content
					FROM userapi_account_datas
					WHERE server_name = $1 AND room_id != '' AND room_id IS NOT NULL
					ORDER BY localpart, room_id, type
				`, m.ServerName)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite room account data: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse room_account_data
				stmt, err := tx.Prepare(`
					INSERT INTO room_account_data (user_id, room_id, account_data_type, stream_id, content, instance_name)
					VALUES ($1, $2, $3, nextval('account_data_sequence'), $4, $5)
					ON CONFLICT (user_id, room_id, account_data_type) DO UPDATE SET
						content = EXCLUDED.content,
						stream_id = EXCLUDED.stream_id
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare room_account_data insert: %w", err)
				}
				defer stmt.Close()

				count := 0
				for rows.Next() {
					var localpart, serverName, roomID, dataType, content string

					if err := rows.Scan(&localpart, &serverName, &roomID, &dataType, &content); err != nil {
						return fmt.Errorf("failed to scan room account data row: %w", err)
					}

					// Convert to full user ID format
					userID := "@" + localpart + ":" + serverName

					// Use 'master' as default instance name
					if _, err := stmt.Exec(userID, roomID, dataType, content, "master"); err != nil {
						return fmt.Errorf("failed to insert room account data for user %s room %s type %s: %w", userID, roomID, dataType, err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading room account data rows: %w", err)
				}

				m.Log.Debugf("Migrated %d room account data entries", count)
				return nil
			},
		},
		{
			Name:        "migrate_push_rules",
			Description: "Migrate push rules from account data",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				m.Log.Info("Migrating push rules from account data...")

				// Query Dendrite for push rules stored as account data
				rows, err := m.DendriteDB.Query(`
					SELECT
						localpart,
						server_name,
						content
					FROM userapi_account_datas
					WHERE server_name = $1
						AND (room_id = '' OR room_id IS NULL)
						AND type = 'm.push_rules'
					ORDER BY localpart
				`, m.ServerName)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite push rules: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse push_rules
				stmt, err := tx.Prepare(`
					INSERT INTO push_rules (user_name, rule_id, priority_class, priority, conditions, actions)
					VALUES ($1, $2, $3, $4, $5, $6)
					ON CONFLICT (user_name, rule_id) DO UPDATE SET
						priority_class = EXCLUDED.priority_class,
						priority = EXCLUDED.priority,
						conditions = EXCLUDED.conditions,
						actions = EXCLUDED.actions
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare push_rules insert: %w", err)
				}
				defer stmt.Close()

				count := 0
				for rows.Next() {
					var localpart, serverName, content string

					if err := rows.Scan(&localpart, &serverName, &content); err != nil {
						return fmt.Errorf("failed to scan push rules row: %w", err)
					}

					// Convert to full user ID format
					userID := "@" + localpart + ":" + serverName

					// TODO: Parse the push rules JSON and convert to individual Synapse push_rules rows
					// For now, we'll log that we found push rules but skip the complex JSON parsing
					m.Log.Debugf("Found push rules for user %s (JSON parsing not yet implemented)", userID)
					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading push rules rows: %w", err)
				}

				m.Log.Debugf("Found %d users with push rules (detailed migration pending)", count)
				return nil
			},
		},
		{
			Name:        "migrate_receipts",
			Description: "Migrate read receipts",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				m.Log.Info("Migrating read receipts...")

				// Query Dendrite for receipts
				rows, err := m.DendriteDB.Query(`
					SELECT
						room_id,
						receipt_type,
						user_id,
						event_id,
						receipt_ts
					FROM syncapi_receipts
					WHERE user_id LIKE '%:' || $1
					ORDER BY room_id, user_id, receipt_ts
				`, m.ServerName)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite receipts: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse receipts_linearized
				stmt, err := tx.Prepare(`
					INSERT INTO receipts_linearized (stream_id, room_id, receipt_type, user_id, event_id, data, instance_name, event_stream_ordering, thread_id)
					VALUES (nextval('receipts_sequence'), $1, $2, $3, $4, $5, $6,
					        (SELECT stream_ordering FROM events WHERE event_id = $4 LIMIT 1), $7)
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare receipts_linearized insert: %w", err)
				}
				defer stmt.Close()

				count := 0
				for rows.Next() {
					var roomID, receiptType, userID, eventID string
					var receiptTS int64

					if err := rows.Scan(&roomID, &receiptType, &userID, &eventID, &receiptTS); err != nil {
						return fmt.Errorf("failed to scan receipt row: %w", err)
					}

					// Ensure user ID has @ prefix if missing
					if !strings.HasPrefix(userID, "@") {
						userID = "@" + userID
					}

					// Create receipt data JSON (timestamp)
					receiptData := fmt.Sprintf(`{"ts": %d}`, receiptTS)

					// Use 'master' as default instance name, NULL for thread_id
					if _, err := stmt.Exec(roomID, receiptType, userID, eventID, receiptData, "master", nil); err != nil {
						return fmt.Errorf("failed to insert receipt for user %s room %s: %w", userID, roomID, err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading receipt rows: %w", err)
				}

				m.Log.Debugf("Migrated %d read receipts", count)
				return nil
			},
		},
		{
			Name:        "migrate_user_filters",
			Description: "Migrate user sync filters",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				m.Log.Info("Migrating user sync filters...")

				// Query Dendrite for user filters
				rows, err := m.DendriteDB.Query(`
					SELECT
						id,
						localpart,
						filter
					FROM syncapi_filter
					ORDER BY localpart, id
				`)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite user filters: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse user_filters
				stmt, err := tx.Prepare(`
					INSERT INTO user_filters (user_id, filter_id, filter_json, full_user_id)
					VALUES ($1, $2, $3, $4)
					ON CONFLICT (user_id, filter_id) DO UPDATE SET
						filter_json = EXCLUDED.filter_json,
						full_user_id = EXCLUDED.full_user_id
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare user_filters insert: %w", err)
				}
				defer stmt.Close()

				count := 0
				for rows.Next() {
					var filterID int
					var localpart, filterJSON string

					if err := rows.Scan(&filterID, &localpart, &filterJSON); err != nil {
						return fmt.Errorf("failed to scan user filter row: %w", err)
					}

					// Convert to full user ID format
					userID := "@" + localpart + ":" + m.ServerName

					if _, err := stmt.Exec(userID, filterID, filterJSON, userID); err != nil {
						return fmt.Errorf("failed to insert user filter for user %s: %w", userID, err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading user filter rows: %w", err)
				}

				m.Log.Debugf("Migrated %d user filters", count)
				return nil
			},
		},
		{
			Name:        "migrate_pushers",
			Description: "Migrate push notification configurations",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				m.Log.Info("Migrating push notification configurations...")

				// Query Dendrite for pushers
				rows, err := m.DendriteDB.Query(`
					SELECT
						id,
						localpart,
						server_name,
						session_id,
						profile_tag,
						kind,
						app_id,
						app_display_name,
						device_display_name,
						pushkey,
						pushkey_ts_ms,
						lang,
						data
					FROM userapi_pushers
					WHERE server_name = $1
					ORDER BY localpart, id
				`, m.ServerName)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite pushers: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse pushers
				stmt, err := tx.Prepare(`
					INSERT INTO pushers (
						id, user_name, access_token, profile_tag, kind, app_id,
						app_display_name, device_display_name, pushkey, ts,
						lang, data, last_stream_ordering, last_success, failing_since,
						enabled, device_id
					)
					VALUES (nextval('pushers_sequence'), $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
					ON CONFLICT (app_id, pushkey, user_name) DO UPDATE SET
						access_token = EXCLUDED.access_token,
						profile_tag = EXCLUDED.profile_tag,
						kind = EXCLUDED.kind,
						app_display_name = EXCLUDED.app_display_name,
						device_display_name = EXCLUDED.device_display_name,
						ts = EXCLUDED.ts,
						lang = EXCLUDED.lang,
						data = EXCLUDED.data,
						enabled = EXCLUDED.enabled,
						device_id = EXCLUDED.device_id
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare pushers insert: %w", err)
				}
				defer stmt.Close()

				count := 0
				for rows.Next() {
					var id, sessionID sql.NullInt64
					var localpart, serverName, profileTag, kind, appID, appDisplayName, deviceDisplayName, pushkey, lang, data sql.NullString
					var pushkeyTsMs int64

					if err := rows.Scan(&id, &localpart, &serverName, &sessionID, &profileTag, &kind, &appID, &appDisplayName, &deviceDisplayName, &pushkey, &pushkeyTsMs, &lang, &data); err != nil {
						return fmt.Errorf("failed to scan pusher row: %w", err)
					}

					// Convert to full user ID format
					userID := "@" + localpart.String + ":" + serverName.String

					// Use NULL access token for now (would need to look up actual token)
					var accessToken *int64 = nil
					var lastStreamOrdering *int64 = nil
					var lastSuccess *int64 = nil
					var failingSince *int64 = nil

					// Default enabled to true for migrated pushers, device_id to NULL
					enabled := true
					var deviceID *string = nil

					if _, err := stmt.Exec(
						userID, accessToken,
						&profileTag.String, &kind.String, &appID.String,
						&appDisplayName.String, &deviceDisplayName.String, &pushkey.String,
						pushkeyTsMs, &lang.String, &data.String,
						lastStreamOrdering, lastSuccess, failingSince,
						&enabled, deviceID,
					); err != nil {
						return fmt.Errorf("failed to insert pusher for user %s: %w", userID, err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading pusher rows: %w", err)
				}

				m.Log.Debugf("Migrated %d push notification configurations", count)
				return nil
			},
		},
		{
			Name:        "migrate_cross_signing_keys",
			Description: "Migrate cross-signing keys for device verification",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				m.Log.Info("Migrating cross-signing keys...")

				// Query Dendrite for cross-signing keys
				rows, err := m.DendriteDB.Query(`
					SELECT
						user_id,
						key_type,
						key_data
					FROM keyserver_cross_signing_keys
					WHERE user_id LIKE '%:' || $1
					ORDER BY user_id, key_type
				`, m.ServerName)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite cross-signing keys: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse e2e_cross_signing_keys
				stmt, err := tx.Prepare(`
					INSERT INTO e2e_cross_signing_keys (user_id, keytype, keydata, stream_id)
					VALUES ($1, $2, $3, nextval('e2e_cross_signing_keys_sequence'))
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare e2e_cross_signing_keys insert: %w", err)
				}
				defer stmt.Close()

				count := 0
				for rows.Next() {
					var userID, keyData string
					var keyType int

					if err := rows.Scan(&userID, &keyType, &keyData); err != nil {
						return fmt.Errorf("failed to scan cross-signing key row: %w", err)
					}

					// Ensure user ID has @ prefix
					if !strings.HasPrefix(userID, "@") {
						userID = "@" + userID
					}

					// Convert Dendrite key type to Synapse key type string
					var synapseKeyType string
					var usage []string
					switch keyType {
					case 1:
						synapseKeyType = "master"
						usage = []string{"master"}
					case 2:
						synapseKeyType = "self_signing"
						usage = []string{"self_signing"}
					case 3:
						synapseKeyType = "user_signing"
						usage = []string{"user_signing"}
					default:
						m.Log.Warnf("Unknown key type %d for user %s, skipping", keyType, userID)
						continue
					}

					// Reconstruct the full JSON structure that Synapse expects
					// Dendrite stores only the base64 key, but Synapse needs the complete JSON object
					keyID := "ed25519:" + keyData

					// Look up signatures for this key from the cross-signing signatures table
					signatures, err := m.getCrossSigningSignatures(tx, userID, keyID)
					if err != nil {
						return fmt.Errorf("failed to get signatures for key %s: %w", keyID, err)
					}

					crossSigningKey := map[string]any{
						"user_id": userID,
						"usage":   usage,
						"keys": map[string]string{
							keyID: keyData,
						},
						"signatures": signatures,
					}

					// Convert to JSON string
					keyDataJSON, err := json.Marshal(crossSigningKey)
					if err != nil {
						return fmt.Errorf("failed to marshal cross-signing key JSON for user %s: %w", userID, err)
					}

					if _, err := stmt.Exec(userID, synapseKeyType, string(keyDataJSON)); err != nil {
						return fmt.Errorf("failed to insert cross-signing key for user %s: %w", userID, err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading cross-signing key rows: %w", err)
				}

				m.Log.Debugf("Migrated %d cross-signing keys", count)
				return nil
			},
		},
		{
			Name:        "migrate_cross_signing_signatures",
			Description: "Migrate cross-signing signatures",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				m.Log.Info("Migrating cross-signing signatures...")

				// Query Dendrite for cross-signing signatures
				rows, err := m.DendriteDB.Query(`
					SELECT
						origin_user_id,
						origin_key_id,
						target_user_id,
						target_key_id,
						signature
					FROM keyserver_cross_signing_sigs
					WHERE origin_user_id LIKE '%:' || $1 OR target_user_id LIKE '%:' || $1
					ORDER BY origin_user_id, target_user_id
				`, m.ServerName)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite cross-signing signatures: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse e2e_cross_signing_signatures
				stmt, err := tx.Prepare(`
					INSERT INTO e2e_cross_signing_signatures (user_id, key_id, target_user_id, target_device_id, signature)
					VALUES ($1, $2, $3, $4, $5)
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare e2e_cross_signing_signatures insert: %w", err)
				}
				defer stmt.Close()

				count := 0
				for rows.Next() {
					var originUserID, originKeyID, targetUserID, targetKeyID, signature string

					if err := rows.Scan(&originUserID, &originKeyID, &targetUserID, &targetKeyID, &signature); err != nil {
						return fmt.Errorf("failed to scan cross-signing signature row: %w", err)
					}

					// Ensure user IDs have @ prefix
					if !strings.HasPrefix(originUserID, "@") {
						originUserID = "@" + originUserID
					}
					if !strings.HasPrefix(targetUserID, "@") {
						targetUserID = "@" + targetUserID
					}

					if _, err := stmt.Exec(originUserID, originKeyID, targetUserID, targetKeyID, signature); err != nil {
						return fmt.Errorf("failed to insert cross-signing signature: %w", err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading cross-signing signature rows: %w", err)
				}

				m.Log.Debugf("Migrated %d cross-signing signatures", count)
				return nil
			},
		},
		{
			Name:        "migrate_device_keys",
			Description: "Migrate device encryption keys",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				m.Log.Info("Migrating device keys...")

				// Query Dendrite for device keys
				rows, err := m.DendriteDB.Query(`
					SELECT
						user_id,
						device_id,
						ts_added_secs,
						key_json,
						stream_id,
						display_name
					FROM keyserver_device_keys
					WHERE user_id LIKE '%:' || $1
					ORDER BY user_id, device_id
				`, m.ServerName)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite device keys: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse e2e_device_keys_json
				stmt, err := tx.Prepare(`
					INSERT INTO e2e_device_keys_json (user_id, device_id, ts_added_ms, key_json)
					VALUES ($1, $2, $3, $4)
					ON CONFLICT (user_id, device_id) DO UPDATE SET
						ts_added_ms = EXCLUDED.ts_added_ms,
						key_json = EXCLUDED.key_json
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare e2e_device_keys_json insert: %w", err)
				}
				defer stmt.Close()

				count := 0
				for rows.Next() {
					var userID, deviceID, keyJSON string
					var tsAddedSecs, streamID int64
					var displayName sql.NullString

					if err := rows.Scan(&userID, &deviceID, &tsAddedSecs, &keyJSON, &streamID, &displayName); err != nil {
						return fmt.Errorf("failed to scan device key row: %w", err)
					}

					// Ensure user ID has @ prefix
					if !strings.HasPrefix(userID, "@") {
						userID = "@" + userID
					}

					// Convert seconds to milliseconds
					tsAddedMs := tsAddedSecs * 1000

					if _, err := stmt.Exec(userID, deviceID, tsAddedMs, keyJSON); err != nil {
						return fmt.Errorf("failed to insert device key for user %s device %s: %w", userID, deviceID, err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading device key rows: %w", err)
				}

				m.Log.Debugf("Migrated %d device keys", count)
				return nil
			},
		},
		{
			Name:        "migrate_one_time_keys",
			Description: "Migrate one-time encryption keys",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				m.Log.Info("Migrating one-time keys...")

				// Query Dendrite for one-time keys (excluding fallback keys)
				rows, err := m.DendriteDB.Query(`
					SELECT
						user_id,
						device_id,
						key_id,
						algorithm,
						ts_added_secs,
						key_json
					FROM keyserver_one_time_keys
					WHERE user_id LIKE '%:' || $1
					  AND algorithm != 'fallback'
					ORDER BY user_id, device_id, key_id
				`, m.ServerName)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite one-time keys: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse e2e_one_time_keys_json
				stmt, err := tx.Prepare(`
					INSERT INTO e2e_one_time_keys_json (user_id, device_id, algorithm, key_id, ts_added_ms, key_json)
					VALUES ($1, $2, $3, $4, $5, $6)
					ON CONFLICT (user_id, device_id, algorithm, key_id) DO UPDATE SET
						ts_added_ms = EXCLUDED.ts_added_ms,
						key_json = EXCLUDED.key_json
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare e2e_one_time_keys_json insert: %w", err)
				}
				defer stmt.Close()

				count := 0
				for rows.Next() {
					var userID, deviceID, keyID, algorithm, keyJSON string
					var tsAddedSecs int64

					if err := rows.Scan(&userID, &deviceID, &keyID, &algorithm, &tsAddedSecs, &keyJSON); err != nil {
						return fmt.Errorf("failed to scan one-time key row: %w", err)
					}

					// Ensure user ID has @ prefix
					if !strings.HasPrefix(userID, "@") {
						userID = "@" + userID
					}

					// Convert seconds to milliseconds
					tsAddedMs := tsAddedSecs * 1000

					if _, err := stmt.Exec(userID, deviceID, algorithm, keyID, tsAddedMs, keyJSON); err != nil {
						return fmt.Errorf("failed to insert one-time key for user %s device %s: %w", userID, deviceID, err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading one-time key rows: %w", err)
				}

				m.Log.Debugf("Migrated %d one-time keys", count)
				return nil
			},
		},
		{
			Name:        "migrate_fallback_keys",
			Description: "Migrate fallback encryption keys",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				m.Log.Info("Migrating fallback keys...")

				// Query Dendrite for fallback keys
				rows, err := m.DendriteDB.Query(`
					SELECT
						user_id,
						device_id,
						key_id,
						algorithm,
						ts_added_secs,
						key_json
					FROM keyserver_one_time_keys
					WHERE user_id LIKE '%:' || $1
					  AND algorithm = 'fallback'
					ORDER BY user_id, device_id, key_id
				`, m.ServerName)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite fallback keys: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse e2e_fallback_keys_json
				stmt, err := tx.Prepare(`
					INSERT INTO e2e_fallback_keys_json (user_id, device_id, algorithm, key_id, key_json, used)
					VALUES ($1, $2, $3, $4, $5, $6)
					ON CONFLICT (user_id, device_id, algorithm) DO UPDATE SET
						key_id = EXCLUDED.key_id,
						key_json = EXCLUDED.key_json,
						used = EXCLUDED.used
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare e2e_fallback_keys_json insert: %w", err)
				}
				defer stmt.Close()

				count := 0
				for rows.Next() {
					var userID, deviceID, keyID, algorithm, keyJSON string
					var tsAddedSecs int64

					if err := rows.Scan(&userID, &deviceID, &keyID, &algorithm, &tsAddedSecs, &keyJSON); err != nil {
						return fmt.Errorf("failed to scan fallback key row: %w", err)
					}

					// Ensure user ID has @ prefix
					if !strings.HasPrefix(userID, "@") {
						userID = "@" + userID
					}

					// Default used to false for migrated fallback keys
					used := false

					if _, err := stmt.Exec(userID, deviceID, algorithm, keyID, keyJSON, used); err != nil {
						return fmt.Errorf("failed to insert fallback key for user %s device %s: %w", userID, deviceID, err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading fallback key rows: %w", err)
				}

				m.Log.Debugf("Migrated %d fallback keys", count)
				return nil
			},
		},
		{
			Name:        "migrate_key_backup_versions",
			Description: "Migrate key backup versions",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				m.Log.Info("Migrating key backup versions...")

				// Query Dendrite for key backup versions
				rows, err := m.DendriteDB.Query(`
					SELECT
						user_id,
						version,
						algorithm,
						auth_data,
						etag,
						deleted
					FROM userapi_key_backup_versions
					WHERE user_id LIKE '%:' || $1
					ORDER BY user_id, version
				`, m.ServerName)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite key backup versions: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse e2e_room_keys_versions
				stmt, err := tx.Prepare(`
					INSERT INTO e2e_room_keys_versions (user_id, version, algorithm, auth_data, deleted, etag)
					VALUES ($1, $2, $3, $4, $5, $6)
					ON CONFLICT (user_id, version) DO UPDATE SET
						algorithm = EXCLUDED.algorithm,
						auth_data = EXCLUDED.auth_data,
						deleted = EXCLUDED.deleted,
						etag = EXCLUDED.etag
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare e2e_room_keys_versions insert: %w", err)
				}
				defer stmt.Close()

				count := 0
				for rows.Next() {
					var userID, algorithm, authData, etag string
					var version, deleted int64

					if err := rows.Scan(&userID, &version, &algorithm, &authData, &etag, &deleted); err != nil {
						return fmt.Errorf("failed to scan key backup version row: %w", err)
					}

					// Ensure user ID has @ prefix
					if !strings.HasPrefix(userID, "@") {
						userID = "@" + userID
					}

					// Keep deleted as smallint (0 or 1)
					deletedInt := int16(deleted)

					if _, err := stmt.Exec(userID, version, algorithm, authData, deletedInt, etag); err != nil {
						return fmt.Errorf("failed to insert key backup version for user %s: %w", userID, err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading key backup version rows: %w", err)
				}

				m.Log.Debugf("Migrated %d key backup versions", count)
				return nil
			},
		},
		{
			Name:        "migrate_key_backups",
			Description: "Migrate encrypted key backup data",
			Run: func(step MigrationStep, tx *sql.Tx) error {
				m.Log.Info("Migrating key backup data...")

				// Query Dendrite for key backup data
				rows, err := m.DendriteDB.Query(`
					SELECT
						user_id,
						room_id,
						session_id,
						version,
						first_message_index,
						forwarded_count,
						is_verified,
						session_data
					FROM userapi_key_backups
					WHERE user_id LIKE '%:' || $1
					ORDER BY user_id, room_id, session_id
				`, m.ServerName)
				if err != nil {
					return fmt.Errorf("failed to query Dendrite key backups: %w", err)
				}
				defer rows.Close()

				// Prepare insert statement for Synapse e2e_room_keys
				stmt, err := tx.Prepare(`
					INSERT INTO e2e_room_keys (user_id, room_id, session_id, version, first_message_index, forwarded_count, is_verified, session_data)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
					ON CONFLICT (user_id, version, room_id, session_id) DO UPDATE SET
						first_message_index = EXCLUDED.first_message_index,
						forwarded_count = EXCLUDED.forwarded_count,
						is_verified = EXCLUDED.is_verified,
						session_data = EXCLUDED.session_data
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare e2e_room_keys insert: %w", err)
				}
				defer stmt.Close()

				count := 0
				for rows.Next() {
					var userID, roomID, sessionID, sessionData string
					var version int64
					var firstMessageIndex, forwardedCount int
					var isVerified bool

					if err := rows.Scan(&userID, &roomID, &sessionID, &version, &firstMessageIndex, &forwardedCount, &isVerified, &sessionData); err != nil {
						return fmt.Errorf("failed to scan key backup row: %w", err)
					}

					// Ensure user ID has @ prefix
					if !strings.HasPrefix(userID, "@") {
						userID = "@" + userID
					}

					if _, err := stmt.Exec(userID, roomID, sessionID, version, firstMessageIndex, forwardedCount, isVerified, sessionData); err != nil {
						return fmt.Errorf("failed to insert key backup for user %s room %s: %w", userID, roomID, err)
					}

					count++
				}

				if err := rows.Err(); err != nil {
					return fmt.Errorf("error reading key backup rows: %w", err)
				}

				m.Log.Debugf("Migrated %d key backup entries", count)
				return nil
			},
		},
		{
			Name:        "update_sequences",
			Description: "Update sequence generators",
			Run: m.newSynapseCommand(`
				-- Update sequence generators to continue from Dendrite's highest values
				SELECT setval(
					'events_stream_seq',
					COALESCE((SELECT MAX(stream_ordering) FROM events), 1)
				);

				-- Update stream positions table
				INSERT INTO stream_positions (stream_name, instance_name, stream_id)
				VALUES
					('events', 'master', (SELECT COALESCE(MAX(stream_ordering), 1) FROM events))
				ON CONFLICT (stream_name, instance_name) DO UPDATE SET
					stream_id = EXCLUDED.stream_id;
			`),
		},
	}
}

// getCrossSigningSignatures looks up signatures for a cross-signing key
func (m *Migrator) getCrossSigningSignatures(tx *sql.Tx, userID, keyID string) (map[string]any, error) {
	// Query for signatures where this key is the target (i.e., signatures ON this key)
	rows, err := m.DendriteDB.Query(`
		SELECT
			origin_user_id,
			origin_key_id,
			signature
		FROM keyserver_cross_signing_sigs
		WHERE target_user_id = $1 AND target_key_id = $2
		ORDER BY origin_user_id, origin_key_id
	`, userID, keyID)
	if err != nil {
		return nil, fmt.Errorf("failed to query signatures for key %s: %w", keyID, err)
	}
	defer rows.Close()

	signatures := make(map[string]any)

	for rows.Next() {
		var originUserID, originKeyID, signature string
		if err := rows.Scan(&originUserID, &originKeyID, &signature); err != nil {
			return nil, fmt.Errorf("failed to scan signature row: %w", err)
		}

		// Ensure origin user ID has @ prefix
		if !strings.HasPrefix(originUserID, "@") {
			originUserID = "@" + originUserID
		}

		// Build the nested signature structure
		if signatures[originUserID] == nil {
			signatures[originUserID] = make(map[string]string)
		}

		userSigs := signatures[originUserID].(map[string]string)
		userSigs[originKeyID] = signature
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error reading signature rows: %w", err)
	}

	return signatures, nil
}
