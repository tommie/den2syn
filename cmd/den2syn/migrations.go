package main

import (
	"database/sql"
	"fmt"
	"strings"
)

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
			Description: "User Account Migration",
			Steps:       m.getUserAccountMigrationSteps(),
		},
		{
			Name:        "Phase 4",
			Description: "Room Migration",
			Steps:       m.getRoomMigrationSteps(),
		},
		{
			Name:        "Phase 5",
			Description: "State and Membership Migration",
			Steps:       m.getStateMigrationSteps(),
		},
		{
			Name:        "Phase 6",
			Description: "End-to-End Encryption Migration",
			Steps:       m.getEncryptionMigrationSteps(),
		},
		{
			Name:        "Phase 7",
			Description: "Account Data Migration",
			Steps:       m.getAccountDataMigrationSteps(),
		},
		{
			Name:        "Phase 8",
			Description: "Miscellaneous Sync Migration",
			Steps:       m.getSyncMigrationSteps(),
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

func (m *Migrator) getSyncMigrationSteps() []MigrationStep {
	return []MigrationStep{
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
