package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

func (m *Migrator) getEncryptionMigrationSteps() []MigrationStep {
	return []MigrationStep{
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
					var tsAddedSecs int64
					var streamID sql.NullInt64
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
						auth_data
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
						deleted = EXCLUDED.deleted
				`)
				if err != nil {
					return fmt.Errorf("failed to prepare e2e_room_keys_versions insert: %w", err)
				}
				defer stmt.Close()

				count := 0
				for rows.Next() {
					var userID, algorithm, authData string
					var version int64

					if err := rows.Scan(&userID, &version, &algorithm, &authData); err != nil {
						return fmt.Errorf("failed to scan key backup version row: %w", err)
					}

					// Ensure user ID has @ prefix
					if !strings.HasPrefix(userID, "@") {
						userID = "@" + userID
					}

					// Set default values for Synapse-specific fields
					deleted := 0
					etag := int64(1) // Simple etag for migrated data

					if _, err := stmt.Exec(userID, version, algorithm, authData, deleted, etag); err != nil {
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
	`, userID, keyID)
	if err != nil {
		return nil, fmt.Errorf("error querying signatures: %w", err)
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

		// Build nested signature structure: signatures[user_id][key_id] = signature
		if signatures[originUserID] == nil {
			signatures[originUserID] = make(map[string]string)
		}
		signatures[originUserID].(map[string]string)[originKeyID] = signature
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error reading signature rows: %w", err)
	}

	return signatures, nil
}