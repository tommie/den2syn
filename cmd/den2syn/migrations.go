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
			Name:        "list_all_tables",
			Description: "List all available tables for debugging",
			StepType:    "dendrite_query",
			Query: `
				SELECT
					schemaname,
					tablename
				FROM pg_tables
				ORDER BY schemaname, tablename;
			`,
		},
		{
			Name:        "check_required_tables",
			Description: "Check for required Dendrite tables",
			StepType:    "dendrite_query",
			Query: `
				SELECT
					tablename as found_table,
					'Table exists: ' || tablename as status
				FROM pg_tables
				WHERE tablename IN (
					'roomserver_events', 'roomserver_event_json', 'roomserver_rooms',
					'userapi_accounts', 'userapi_devices', 'syncapi_output_room_events'
				)
				ORDER BY tablename;
			`,
		},
		{
			Name:        "analyze_event_coverage",
			Description: "Check event coverage and missing event_json",
			StepType:    "dendrite_query",
			Query: `
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
			`,
		},
		{
			Name:        "validate_table_count",
			Description: "Validate all required tables are present",
			StepType:    "dendrite_query",
			Query: `
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
			`,
		},
		{
			Name:        "integrity_check",
			Description: "Check data integrity",
			StepType:    "dendrite_query",
			Query: `
				SELECT
					COUNT(*) as orphaned_events,
					'Orphaned events (events without JSON)' as description
				FROM roomserver_events e
				LEFT JOIN roomserver_event_json ej ON e.event_nid = ej.event_nid
				WHERE ej.event_nid IS NULL;
			`,
		},
	}
}

func (m *Migrator) getNIDResolutionSteps() []MigrationStep {
	return []MigrationStep{
		{
			Name:        "create_mapping_tables",
			Description: "Create NID mapping tables",
			StepType:    "synapse_command",
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
			StepType:    "migration",
			Query:       "populate_room_mapping", // Special migration identifier
		},
		{
			Name:        "populate_event_mapping",
			Description: "Populate event NID mappings",
			StepType:    "migration",
			Query:       "populate_event_mapping", // Special migration identifier
		},
		{
			Name:        "populate_eventtype_mapping",
			Description: "Populate event type NID mappings",
			StepType:    "migration",
			Query:       "populate_eventtype_mapping", // Special migration identifier
		},
		{
			Name:        "populate_statekey_mapping",
			Description: "Populate state key NID mappings",
			StepType:    "migration",
			Query:       "populate_statekey_mapping", // Special migration identifier
		},
	}
}

func (m *Migrator) getCoreDataMigrationSteps() []MigrationStep {
	return []MigrationStep{
		{
			Name:        "migrate_users",
			Description: "Migrate user accounts",
			StepType:    "migration",
			Query:       "migrate_users", // Special migration identifier
		},
		{
			Name:        "migrate_devices",
			Description: "Migrate user devices",
			StepType:    "migration",
			Query:       "migrate_devices", // Special migration identifier
		},
		{
			Name:        "migrate_access_tokens",
			Description: "Migrate access tokens",
			StepType:    "migration",
			Query:       "migrate_access_tokens", // Special migration identifier
		},
		{
			Name:        "migrate_rooms",
			Description: "Migrate rooms",
			StepType:    "migration",
			Query:       "migrate_rooms", // Special migration identifier
		},
		{
			Name:        "migrate_events",
			Description: "Migrate events",
			StepType:    "migration",
			Query:       "migrate_events", // Special migration identifier
		},
		{
			Name:        "migrate_event_json",
			Description: "Migrate event JSON",
			StepType:    "migration",
			Query:       "migrate_event_json", // Special migration identifier
		},
	}
}

func (m *Migrator) getStateMigrationSteps() []MigrationStep {
	return []MigrationStep{
		{
			Name:        "migrate_current_state",
			Description: "Migrate current room state",
			StepType:    "migration",
			Query:       "migrate_current_state", // Special migration identifier
		},
		{
			Name:        "migrate_room_memberships",
			Description: "Migrate room memberships",
			StepType:    "migration",
			Query:       "migrate_room_memberships", // Special migration identifier
		},
		{
			Name:        "migrate_local_current_membership",
			Description: "Migrate local current membership for room visibility",
			StepType:    "migration",
			Query:       "migrate_local_current_membership", // Special migration identifier
		},
		{
			Name:        "migrate_state_groups",
			Description: "Migrate state groups for proper state resolution",
			StepType:    "migration",
			Query:       "migrate_state_groups", // Special migration identifier
		},
		{
			Name:        "migrate_event_to_state_groups",
			Description: "Map events to state groups",
			StepType:    "migration",
			Query:       "migrate_event_to_state_groups", // Special migration identifier
		},
	}
}

func (m *Migrator) getAdvancedFeaturesSteps() []MigrationStep {
	return []MigrationStep{
		{
			Name:        "migrate_profiles",
			Description: "Migrate user profiles (display names and avatars)",
			StepType:    "migration",
			Query:       "migrate_profiles", // Special migration identifier
		},
		{
			Name:        "migrate_cross_signing_keys",
			Description: "Migrate cross-signing keys for device verification",
			StepType:    "migration",
			Query:       "migrate_cross_signing_keys", // Special migration identifier
		},
		{
			Name:        "migrate_cross_signing_signatures",
			Description: "Migrate cross-signing signatures",
			StepType:    "migration",
			Query:       "migrate_cross_signing_signatures", // Special migration identifier
		},
		{
			Name:        "migrate_device_keys",
			Description: "Migrate device encryption keys",
			StepType:    "migration",
			Query:       "migrate_device_keys", // Special migration identifier
		},
		{
			Name:        "migrate_one_time_keys",
			Description: "Migrate one-time encryption keys",
			StepType:    "migration",
			Query:       "migrate_one_time_keys", // Special migration identifier
		},
		{
			Name:        "migrate_fallback_keys",
			Description: "Migrate fallback encryption keys",
			StepType:    "migration",
			Query:       "migrate_fallback_keys", // Special migration identifier
		},
		{
			Name:        "migrate_key_backup_versions",
			Description: "Migrate key backup versions",
			StepType:    "migration",
			Query:       "migrate_key_backup_versions", // Special migration identifier
		},
		{
			Name:        "migrate_key_backups",
			Description: "Migrate encrypted key backup data",
			StepType:    "migration",
			Query:       "migrate_key_backups", // Special migration identifier
		},
		{
			Name:        "migrate_account_data",
			Description: "Migrate global account data",
			StepType:    "migration",
			Query:       "migrate_account_data", // Special migration identifier
		},
		{
			Name:        "migrate_room_account_data",
			Description: "Migrate room-specific account data",
			StepType:    "migration",
			Query:       "migrate_room_account_data", // Special migration identifier
		},
		{
			Name:        "migrate_push_rules",
			Description: "Migrate push rules from account data",
			StepType:    "migration",
			Query:       "migrate_push_rules", // Special migration identifier
		},
		{
			Name:        "migrate_receipts",
			Description: "Migrate read receipts",
			StepType:    "migration",
			Query:       "migrate_receipts", // Special migration identifier
		},
		{
			Name:        "migrate_user_filters",
			Description: "Migrate user sync filters",
			StepType:    "migration",
			Query:       "migrate_user_filters", // Special migration identifier
		},
		{
			Name:        "migrate_pushers",
			Description: "Migrate push notification configurations",
			StepType:    "migration",
			Query:       "migrate_pushers", // Special migration identifier
		},
		{
			Name:        "update_sequences",
			Description: "Update sequence generators",
			StepType:    "synapse_command",
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
