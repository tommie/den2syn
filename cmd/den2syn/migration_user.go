package main

import (
	"database/sql"
	"fmt"
)

func (m *Migrator) getUserAccountMigrationSteps() []MigrationStep {
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
	}
}
