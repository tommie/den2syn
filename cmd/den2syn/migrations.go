package main

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
			Name:        "schema_validation",
			Description: "Validate database schemas",
			Query: `
				-- Verify Dendrite tables exist
				DO $$
				DECLARE
					missing_tables TEXT[];
					required_tables TEXT[] := ARRAY[
						'roomserver_events', 'roomserver_event_json', 'roomserver_rooms',
						'userapi_accounts', 'userapi_devices', 'syncapi_output_room_events'
					];
					table_name TEXT;
				BEGIN
					FOREACH table_name IN ARRAY required_tables LOOP
						IF NOT EXISTS (
							SELECT 1 FROM information_schema.tables 
							WHERE table_schema = 'public' 
							AND table_name = table_name
						) THEN
							missing_tables := array_append(missing_tables, table_name);
						END IF;
					END LOOP;
					
					IF array_length(missing_tables, 1) > 0 THEN
						RAISE EXCEPTION 'Missing required Dendrite tables: %', array_to_string(missing_tables, ', ');
					END IF;
				END $$;
			`,
		},
		{
			Name:        "integrity_check",
			Description: "Check data integrity",
			Query: `
				-- Check for orphaned events
				DO $$
				DECLARE
					orphaned_count INTEGER;
				BEGIN
					SELECT COUNT(*) INTO orphaned_count
					FROM roomserver_events e 
					LEFT JOIN roomserver_event_json ej ON e.event_nid = ej.event_nid 
					WHERE ej.event_nid IS NULL;
					
					IF orphaned_count > 0 THEN
						RAISE WARNING 'Found % orphaned events without JSON content', orphaned_count;
					END IF;
				END $$;
			`,
		},
	}
}

func (m *Migrator) getNIDResolutionSteps() []MigrationStep {
	return []MigrationStep{
		{
			Name:        "create_mapping_tables",
			Description: "Create NID mapping tables",
			Query: `
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
			`,
		},
		{
			Name:        "populate_room_mapping",
			Description: "Populate room NID mappings",
			Query: `
				INSERT INTO nid_room_mapping (room_nid, room_id)
				SELECT room_nid, room_id FROM roomserver_rooms
				ON CONFLICT (room_nid) DO NOTHING;
			`,
		},
		{
			Name:        "populate_event_mapping",
			Description: "Populate event NID mappings",
			Query: `
				INSERT INTO nid_event_mapping (event_nid, event_id)
				SELECT event_nid, event_id FROM roomserver_events
				ON CONFLICT (event_nid) DO NOTHING;
			`,
		},
		{
			Name:        "populate_eventtype_mapping",
			Description: "Populate event type NID mappings",
			Query: `
				INSERT INTO nid_eventtype_mapping (event_type_nid, event_type)
				SELECT event_type_nid, event_type FROM roomserver_event_types
				ON CONFLICT (event_type_nid) DO NOTHING;
			`,
		},
		{
			Name:        "populate_statekey_mapping",
			Description: "Populate state key NID mappings",
			Query: `
				INSERT INTO nid_statekey_mapping (event_state_key_nid, event_state_key)
				SELECT event_state_key_nid, COALESCE(event_state_key, '') 
				FROM roomserver_event_state_keys
				ON CONFLICT (event_state_key_nid) DO NOTHING;
			`,
		},
	}
}

func (m *Migrator) getCoreDataMigrationSteps() []MigrationStep {
	return []MigrationStep{
		{
			Name:        "migrate_users",
			Description: "Migrate user accounts",
			Query: `
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
				SELECT 
					localpart || ':' || server_name as name,
					password_hash,
					created_ts,
					CASE WHEN account_type = 1 THEN 1 ELSE 0 END as admin,
					CASE WHEN account_type = 2 THEN 1 ELSE 0 END as is_guest,
					COALESCE(is_deactivated, false) as deactivated,
					false as shadow_banned,
					COALESCE(account_type, 0) as user_type,
					appservice_id
				FROM userapi_accounts
				ON CONFLICT (name) DO UPDATE SET
					password_hash = EXCLUDED.password_hash,
					creation_ts = EXCLUDED.creation_ts,
					admin = EXCLUDED.admin,
					is_guest = EXCLUDED.is_guest,
					deactivated = EXCLUDED.deactivated,
					user_type = EXCLUDED.user_type,
					appservice_id = EXCLUDED.appservice_id;
			`,
		},
		{
			Name:        "migrate_devices",
			Description: "Migrate user devices",
			Query: `
				INSERT INTO devices (
					user_id,
					device_id,
					display_name,
					last_seen,
					ip,
					user_agent,
					hidden
				)
				SELECT 
					d.localpart || ':' || d.server_name as user_id,
					d.device_id,
					d.display_name,
					d.last_seen_ts,
					d.ip,
					d.user_agent,
					false as hidden
				FROM userapi_devices d
				JOIN userapi_accounts a ON d.localpart = a.localpart AND d.server_name = a.server_name
				ON CONFLICT (user_id, device_id) DO UPDATE SET
					display_name = EXCLUDED.display_name,
					last_seen = EXCLUDED.last_seen,
					ip = EXCLUDED.ip,
					user_agent = EXCLUDED.user_agent;
			`,
		},
		{
			Name:        "migrate_access_tokens",
			Description: "Migrate access tokens",
			Query: `
				INSERT INTO access_tokens (
					user_id,
					device_id,
					token,
					valid_until_ms,
					puppets_user_id,
					last_validated,
					refresh_token_id,
					used
				)
				SELECT 
					d.localpart || ':' || d.server_name as user_id,
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
				ON CONFLICT (token) DO NOTHING;
			`,
		},
		{
			Name:        "migrate_rooms",
			Description: "Migrate rooms",
			Query: `
				INSERT INTO rooms (
					room_id,
					is_public,
					creator,
					room_version,
					has_auth_chain_index
				)
				SELECT 
					r.room_id,
					false as is_public,
					'' as creator, -- Will be updated from create events
					COALESCE(r.room_version, '1') as room_version,
					false as has_auth_chain_index
				FROM roomserver_rooms r
				ON CONFLICT (room_id) DO UPDATE SET
					room_version = EXCLUDED.room_version;
			`,
		},
		{
			Name:        "migrate_events",
			Description: "Migrate events",
			Query: `
				WITH event_data AS (
					SELECT 
						e.event_id,
						etm.event_type as type,
						rm.room_id,
						COALESCE(ej.event_json::jsonb->>'content', '{}')::jsonb as content,
						e.depth,
						ROW_NUMBER() OVER (ORDER BY e.event_nid) as stream_ordering,
						EXTRACT(EPOCH FROM NOW()) * 1000 as received_ts,
						COALESCE(ej.event_json::jsonb->>'sender', '') as sender,
						(ej.event_json LIKE '%"url"%') as contains_url,
						CASE 
							WHEN skm.event_state_key = '' THEN ''
							WHEN skm.event_state_key IS NULL THEN NULL
							ELSE skm.event_state_key
						END as state_key,
						CASE WHEN e.is_rejected THEN 'unknown' ELSE NULL END as rejection_reason,
						COALESCE((ej.event_json::jsonb->>'origin_server_ts')::bigint, 
								EXTRACT(EPOCH FROM NOW()) * 1000) as origin_server_ts
					FROM roomserver_events e
					JOIN nid_room_mapping rm ON e.room_nid = rm.room_nid
					JOIN nid_eventtype_mapping etm ON e.event_type_nid = etm.event_type_nid
					JOIN nid_statekey_mapping skm ON e.event_state_key_nid = skm.event_state_key_nid
					JOIN roomserver_event_json ej ON e.event_nid = ej.event_nid
				)
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
				SELECT 
					stream_ordering as topological_ordering, -- Simplified for now
					event_id,
					type,
					room_id,
					content,
					NULL as unrecognized_keys,
					true as processed,
					false as outlier,
					depth,
					stream_ordering,
					received_ts,
					sender,
					contains_url,
					state_key,
					rejection_reason,
					'master' as instance_name,
					origin_server_ts
				FROM event_data
				ON CONFLICT (event_id) DO UPDATE SET
					processed = EXCLUDED.processed,
					outlier = EXCLUDED.outlier;
			`,
		},
		{
			Name:        "migrate_event_json",
			Description: "Migrate event JSON",
			Query: `
				INSERT INTO event_json (
					event_id,
					room_id,
					internal_metadata,
					json,
					format_version
				)
				SELECT 
					e.event_id,
					rm.room_id,
					'{}' as internal_metadata,
					ej.event_json as json,
					NULL as format_version
				FROM roomserver_events e
				JOIN nid_room_mapping rm ON e.room_nid = rm.room_nid
				JOIN roomserver_event_json ej ON e.event_nid = ej.event_nid
				ON CONFLICT (event_id) DO UPDATE SET
					json = EXCLUDED.json;
			`,
		},
	}
}

func (m *Migrator) getStateMigrationSteps() []MigrationStep {
	return []MigrationStep{
		{
			Name:        "migrate_current_state",
			Description: "Migrate current room state",
			Query: `
				-- This is a simplified version - real implementation would need proper state resolution
				INSERT INTO current_state_events (
					event_id,
					room_id,
					type,
					state_key,
					membership
				)
				SELECT DISTINCT ON (rm.room_id, etm.event_type, skm.event_state_key)
					e.event_id,
					rm.room_id,
					etm.event_type as type,
					skm.event_state_key as state_key,
					CASE 
						WHEN etm.event_type = 'm.room.member' THEN 
							COALESCE(ej.event_json::jsonb->>'membership', 'leave')
						ELSE NULL 
					END as membership
				FROM roomserver_events e
				JOIN nid_room_mapping rm ON e.room_nid = rm.room_nid
				JOIN nid_eventtype_mapping etm ON e.event_type_nid = etm.event_type_nid
				JOIN nid_statekey_mapping skm ON e.event_state_key_nid = skm.event_state_key_nid
				JOIN roomserver_event_json ej ON e.event_nid = ej.event_nid
				WHERE skm.event_state_key IS NOT NULL  -- Only state events
				ORDER BY rm.room_id, etm.event_type, skm.event_state_key, e.event_nid DESC
				ON CONFLICT (room_id, type, state_key) DO UPDATE SET
					event_id = EXCLUDED.event_id,
					membership = EXCLUDED.membership;
			`,
		},
		{
			Name:        "migrate_room_memberships",
			Description: "Migrate room memberships",
			Query: `
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
				SELECT 
					e.event_id,
					skm.event_state_key as user_id,
					COALESCE(ej.event_json::jsonb->>'sender', '') as sender,
					rm.room_id,
					COALESCE(ej.event_json::jsonb->>'membership', 'leave') as membership,
					ej.event_json::jsonb->>'display_name' as display_name,
					ej.event_json::jsonb->>'avatar_url' as avatar_url,
					false as forgotten
				FROM roomserver_events e
				JOIN nid_room_mapping rm ON e.room_nid = rm.room_nid
				JOIN nid_eventtype_mapping etm ON e.event_type_nid = etm.event_type_nid
				JOIN nid_statekey_mapping skm ON e.event_state_key_nid = skm.event_state_key_nid
				JOIN roomserver_event_json ej ON e.event_nid = ej.event_nid
				WHERE etm.event_type = 'm.room.member'
				ON CONFLICT (event_id) DO UPDATE SET
					membership = EXCLUDED.membership,
					display_name = EXCLUDED.display_name,
					avatar_url = EXCLUDED.avatar_url;
			`,
		},
	}
}

func (m *Migrator) getAdvancedFeaturesSteps() []MigrationStep {
	return []MigrationStep{
		{
			Name:        "update_sequences",
			Description: "Update sequence generators",
			Query: `
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
			`,
		},
	}
}

// Helper function to create UDF-like functionality in Go
func (m *Migrator) extractEventMetadata(eventJSON string) (map[string]interface{}, error) {
	// This would be implemented to parse JSON and extract metadata
	// For now, return empty map
	return make(map[string]interface{}), nil
}

// Helper function to calculate topological ordering
func (m *Migrator) calculateTopologicalOrdering(eventID, roomID string) (int64, error) {
	// This would implement proper topological ordering calculation
	// For now, return simple ordering
	return 1, nil
}

// Helper function to build state groups
func (m *Migrator) buildStateGroup(roomID, eventID string) (int64, error) {
	// This would implement state group creation logic
	// For now, return simple group ID
	return 1, nil
}

// Helper function to convert auth events
func (m *Migrator) convertAuthEvents(authEventNIDs []int64) ([]string, error) {
	// This would convert NID arrays to event ID arrays
	// For now, return empty array
	return []string{}, nil
}