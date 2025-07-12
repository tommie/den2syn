package main

import (
	"database/sql"
	"fmt"
)

func (m *Migrator) getAccountDataMigrationSteps() []MigrationStep {
	return []MigrationStep{
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
	}
}
