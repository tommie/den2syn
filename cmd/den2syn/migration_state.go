package main

import (
	"database/sql"
	"fmt"
)

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
