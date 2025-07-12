package main

import (
	"database/sql"
	"fmt"
)

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
