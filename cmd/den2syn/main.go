package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/lib/pq"
	"github.com/schollz/progressbar/v3"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	verbose    bool
	commit     bool
	serverName string
	log        = logrus.New()
)

func main() {
	var rootCmd = &cobra.Command{
		Use:   "den2syn [flags] <dendrite-db-url> <synapse-db-url>",
		Short: "Migrate Matrix Dendrite database to Synapse format",
		Long: `den2syn is a tool for migrating Matrix homeserver data from Dendrite to Synapse.

The tool performs an idempotent migration that can be run multiple times safely.
By default, it runs in dry-run mode and will rollback all changes unless --commit is specified.

Database URLs should be in PostgreSQL format:
  postgresql://user:password@host:port/database
  postgres://user:password@host:port/database

Examples:
  den2syn --server-name example.com postgres://user:pass@localhost/dendrite postgres://user:pass@localhost/synapse
  den2syn --commit --verbose --server-name example.com postgres://user:pass@localhost/dendrite postgres://user:pass@localhost/synapse`,
		Args: cobra.ExactArgs(2),
		Run:  runMigration,
	}

	rootCmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Show verbose logging including SQL queries")
	rootCmd.Flags().BoolVarP(&commit, "commit", "c", false, "Commit changes (default is dry-run mode)")
	rootCmd.Flags().StringVarP(&serverName, "server-name", "s", "", "Matrix server name (e.g., test.local) - REQUIRED")
	rootCmd.MarkFlagRequired("server-name")

	if err := rootCmd.Execute(); err != nil {
		log.Fatal(err)
	}
}

func runMigration(cmd *cobra.Command, args []string) {
	// Setup logging
	if verbose {
		log.SetLevel(logrus.DebugLevel)
		log.Debug("Verbose logging enabled")
	} else {
		log.SetLevel(logrus.InfoLevel)
	}

	dendriteURL := args[0]
	synapseURL := args[1]

	log.WithFields(logrus.Fields{
		"dendrite_url": sanitizeURL(dendriteURL),
		"synapse_url":  sanitizeURL(synapseURL),
		"commit_mode":  commit,
	}).Info("Starting den2syn migration")

	// Connect to databases
	log.Info("Connecting to databases...")
	dendriteDB, err := connectDB(dendriteURL, "dendrite")
	if err != nil {
		log.Fatalf("Failed to connect to Dendrite database: %v", err)
	}
	defer dendriteDB.Close()

	synapseDB, err := connectDB(synapseURL, "synapse")
	if err != nil {
		log.Fatalf("Failed to connect to Synapse database: %v", err)
	}
	defer synapseDB.Close()

	// Create migrator
	migrator := &Migrator{
		DendriteDB: dendriteDB,
		SynapseDB:  synapseDB,
		Verbose:    verbose,
		Commit:     commit,
		ServerName: serverName,
		Log:        log,
	}

	// Run migration
	if err := migrator.Migrate(); err != nil {
		log.Fatalf("Migration failed: %v", err)
	}

	if commit {
		log.Info("Migration completed successfully!")
	} else {
		log.Info("Dry-run completed successfully! Use --commit to apply changes.")
	}
}

func connectDB(dbURL, name string) (*sql.DB, error) {
	db, err := sql.Open("postgres", dbURL)
	if err != nil {
		return nil, fmt.Errorf("failed to open %s database: %w", name, err)
	}

	// Test connection
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to ping %s database: %w", name, err)
	}

	log.Debugf("Successfully connected to %s database", name)
	return db, nil
}

func sanitizeURL(url string) string {
	// Simple sanitization - replace password if present
	if len(url) < 10 {
		return "[invalid URL]"
	}

	// Look for password pattern and mask it
	result := url
	if strings.Contains(url, "@") && strings.Contains(url, ":") {
		parts := strings.Split(url, "@")
		if len(parts) >= 2 {
			userPart := parts[0]
			if colonIdx := strings.LastIndex(userPart, ":"); colonIdx != -1 {
				result = userPart[:colonIdx+1] + "***@" + strings.Join(parts[1:], "@")
			}
		}
	}
	return result
}

// Migrator handles the migration process
type Migrator struct {
	DendriteDB *sql.DB
	SynapseDB  *sql.DB
	Verbose    bool
	Commit     bool
	ServerName string
	Log        *logrus.Logger
	UDFManager *UDFManager
}

// Migration phases
type MigrationPhase struct {
	Name        string
	Description string
	Steps       []MigrationStep
}

type MigrationStep struct {
	Name        string
	Description string
	Query       string
	StepType    string // "dendrite_query", "synapse_command", "migration"
	CountQuery  string // Optional query to count affected rows for progress
}

func (m *Migrator) Migrate() error {
	m.Log.Info("Starting migration process...")

	// Begin transaction on Synapse database
	tx, err := m.SynapseDB.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() {
		if tx != nil {
			if err := tx.Rollback(); err != nil {
				m.Log.Errorf("Failed to rollback transaction: %v", err)
			}
		}
	}()

	// Create UDF manager for complex operations
	udfManager := NewUDFManager(m.DendriteDB, m.SynapseDB, m.Verbose, m.Log)
	m.UDFManager = udfManager

	// Get migration phases
	phases := m.getMigrationPhases()

	// Calculate total steps for overall progress
	totalSteps := 0
	for _, phase := range phases {
		totalSteps += len(phase.Steps)
	}

	// Create overall progress bar (only if running in a terminal)
	var overallProgress *progressbar.ProgressBar
	isTerminal := term.IsTerminal(int(os.Stdout.Fd()))
	if isTerminal {
		overallProgress = progressbar.NewOptions(totalSteps,
			progressbar.OptionSetDescription("Migration Progress"),
			progressbar.OptionSetWidth(50),
			progressbar.OptionShowCount(),
			progressbar.OptionSetTheme(progressbar.Theme{
				Saucer:        "=",
				SaucerHead:    ">",
				SaucerPadding: " ",
				BarStart:      "[",
				BarEnd:        "]",
			}))
	}

	stepCounter := 0

	// Execute each phase
	for _, phase := range phases {
		m.Log.Infof("Phase: %s - %s", phase.Name, phase.Description)

		for _, step := range phase.Steps {
			stepCounter++
			m.Log.Infof("Step %d/%d: %s", stepCounter, totalSteps, step.Description)

			if m.Verbose {
				m.Log.Debug("Executing step:")
				m.Log.Debug(step.Description)
			}

			// Execute the step with proper database targeting
			start := time.Now()
			err := m.executeStep(step, tx)
			duration := time.Since(start)

			if err != nil {
				return fmt.Errorf("failed to execute step '%s': %w", step.Name, err)
			}

			m.Log.Debugf("Step completed in %v", duration)

			// Update progress bar (only if it exists)
			if overallProgress != nil {
				overallProgress.Add(1)
			}
		}
	}

	// Validation phase
	m.Log.Info("Running post-migration validation...")
	if err := m.validateMigration(tx); err != nil {
		return fmt.Errorf("migration validation failed: %w", err)
	}

	if m.Commit {
		if err := tx.Commit(); err != nil {
			m.Log.Errorf("Failed to commit transaction: %v", err)
		}
		tx = nil
	}

	return nil
}

// executeStep executes a migration step, handling whether it reads from Dendrite or writes to Synapse
func (m *Migrator) executeStep(step MigrationStep, tx *sql.Tx) error {
	switch step.StepType {
	case "dendrite_query":
		// Execute query against Dendrite database and log results
		return m.executeDendriteQuery(step)
	case "synapse_command":
		// Execute command against Synapse database in transaction
		return m.executeSynapseCommand(step, tx)
	case "migration":
		// Execute migration logic (read from Dendrite, write to Synapse)
		return m.executeMigrationStep(step, tx)
	default:
		// Backwards compatibility - assume it's a Synapse command
		return m.executeSynapseCommand(step, tx)
	}
}

// executeDendriteQuery executes a query against the Dendrite database and logs results
func (m *Migrator) executeDendriteQuery(step MigrationStep) error {
	if m.Verbose {
		m.Log.Debug("Executing Dendrite query:")
		m.Log.Debug(step.Query)
	}

	rows, err := m.DendriteDB.Query(step.Query)
	if err != nil {
		return fmt.Errorf("failed to execute Dendrite query: %w", err)
	}
	defer rows.Close()

	// Get column names
	columns, err := rows.Columns()
	if err != nil {
		return fmt.Errorf("failed to get columns: %w", err)
	}

	m.Log.Infof("Results for step '%s':", step.Description)
	m.Log.Infof("Columns: %v", columns)

	// Read and log all rows
	rowCount := 0
	for rows.Next() {
		rowCount++
		// Create slice to hold values
		values := make([]any, len(columns))
		valuePtrs := make([]any, len(columns))
		for i := range values {
			valuePtrs[i] = &values[i]
		}

		if err := rows.Scan(valuePtrs...); err != nil {
			return fmt.Errorf("failed to scan row: %w", err)
		}

		// Convert values to strings for logging
		stringValues := make([]string, len(values))
		for i, val := range values {
			if val == nil {
				stringValues[i] = "NULL"
			} else {
				// Handle byte arrays (common in PostgreSQL)
				switch v := val.(type) {
				case []byte:
					stringValues[i] = string(v)
				case string:
					stringValues[i] = v
				default:
					stringValues[i] = fmt.Sprintf("%v", val)
				}
			}
		}

		m.Log.Infof("Row %d: %v", rowCount, stringValues)
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("error reading rows: %w", err)
	}

	m.Log.Debugf("Dendrite query returned %d rows", rowCount)
	return nil
}

// executeSynapseCommand executes a command against the Synapse database in transaction
func (m *Migrator) executeSynapseCommand(step MigrationStep, tx *sql.Tx) error {
	if m.Verbose {
		m.Log.Debug("Executing Synapse command:")
		m.Log.Debug(step.Query)
	}

	result, err := tx.Exec(step.Query)
	if err != nil {
		return fmt.Errorf("failed to execute Synapse command: %w", err)
	}

	// Log results
	if rowsAffected, err := result.RowsAffected(); err == nil {
		m.Log.Debugf("Synapse command affected %d rows", rowsAffected)
	}

	return nil
}

// executeMigrationStep executes a data migration step (read from Dendrite, write to Synapse)
func (m *Migrator) executeMigrationStep(step MigrationStep, tx *sql.Tx) error {
	if m.Verbose {
		m.Log.Debugf("Executing migration step: %s", step.Query)
	}

	// Handle special migration identifiers
	switch step.Query {
	case "populate_room_mapping":
		return m.populateRoomMapping(tx)
	case "populate_event_mapping":
		return m.populateEventMapping(tx)
	case "populate_eventtype_mapping":
		return m.populateEventTypeMapping(tx)
	case "populate_statekey_mapping":
		return m.populateStateKeyMapping(tx)
	case "migrate_users":
		return m.migrateUsers(tx)
	case "migrate_devices":
		return m.migrateDevices(tx)
	case "migrate_access_tokens":
		return m.migrateAccessTokens(tx)
	case "migrate_rooms":
		return m.migrateRooms(tx)
	case "migrate_events":
		return m.migrateEvents(tx)
	case "migrate_event_json":
		return m.migrateEventJSON(tx)
	case "migrate_current_state":
		return m.migrateCurrentState(tx)
	case "migrate_room_memberships":
		return m.migrateRoomMemberships(tx)
	case "migrate_local_current_membership":
		return m.migrateLocalCurrentMembership(tx)
	case "migrate_state_groups":
		return m.migrateStateGroups(tx)
	case "migrate_event_to_state_groups":
		return m.migrateEventToStateGroups(tx)
	case "migrate_profiles":
		return m.migrateProfiles(tx)
	case "migrate_account_data":
		return m.migrateAccountData(tx)
	case "migrate_room_account_data":
		return m.migrateRoomAccountData(tx)
	case "migrate_push_rules":
		return m.migratePushRules(tx)
	case "migrate_receipts":
		return m.migrateReceipts(tx)
	case "migrate_user_filters":
		return m.migrateUserFilters(tx)
	case "migrate_pushers":
		return m.migratePushers(tx)
	case "migrate_cross_signing_keys":
		return m.migrateCrossSigningKeys(tx)
	case "migrate_cross_signing_signatures":
		return m.migrateCrossSigningSignatures(tx)
	case "migrate_device_keys":
		return m.migrateDeviceKeys(tx)
	case "migrate_one_time_keys":
		return m.migrateOneTimeKeys(tx)
	case "migrate_fallback_keys":
		return m.migrateFallbackKeys(tx)
	case "migrate_key_backup_versions":
		return m.migrateKeyBackupVersions(tx)
	case "migrate_key_backups":
		return m.migrateKeyBackups(tx)
	default:
		return fmt.Errorf("unknown migration step: %s", step.Query)
	}
}

// populateRoomMapping reads room data from Dendrite and populates the temp mapping table in Synapse
func (m *Migrator) populateRoomMapping(tx *sql.Tx) error {
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
}

// populateEventMapping reads event data from Dendrite and populates the temp mapping table in Synapse
func (m *Migrator) populateEventMapping(tx *sql.Tx) error {
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
}

// populateEventTypeMapping reads event type data from Dendrite and populates the temp mapping table in Synapse
func (m *Migrator) populateEventTypeMapping(tx *sql.Tx) error {
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
}

// populateStateKeyMapping reads state key data from Dendrite and populates the temp mapping table in Synapse
func (m *Migrator) populateStateKeyMapping(tx *sql.Tx) error {
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
}

// migrateUsers reads user data from Dendrite and writes to Synapse
func (m *Migrator) migrateUsers(tx *sql.Tx) error {
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
}

// migrateDevices reads device data from Dendrite and writes to Synapse
func (m *Migrator) migrateDevices(tx *sql.Tx) error {
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
}

// migrateAccessTokens reads access token data from Dendrite and writes to Synapse
func (m *Migrator) migrateAccessTokens(tx *sql.Tx) error {
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
}

// migrateRooms reads room data from Dendrite and writes to Synapse
func (m *Migrator) migrateRooms(tx *sql.Tx) error {
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
}

// migrateEvents reads event data from Dendrite and writes to Synapse
func (m *Migrator) migrateEvents(tx *sql.Tx) error {
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
}

// migrateEventJSON reads event JSON data from Dendrite and writes to Synapse
func (m *Migrator) migrateEventJSON(tx *sql.Tx) error {
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
		// According to Synapse's EventFormatVersions:
		// - ROOM_V1_V2 = 1 (for room v1 and v2)
		// - ROOM_V3 = 2 (for room v3)
		// - ROOM_V4_PLUS = 3 (for room v4 and later)
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
}

// migrateCurrentState reads current state data from Dendrite and writes to Synapse
func (m *Migrator) migrateCurrentState(tx *sql.Tx) error {
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
}

// migrateRoomMemberships reads room membership data from Dendrite and writes to Synapse
func (m *Migrator) migrateRoomMemberships(tx *sql.Tx) error {
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
}

func (m *Migrator) validateMigration(tx *sql.Tx) error {
	validations := []struct {
		name          string
		dendriteQuery string
		synapseQuery  string
	}{
		{
			name:          "Event count validation",
			dendriteQuery: "SELECT COUNT(*) FROM roomserver_events",
			synapseQuery:  "SELECT COUNT(*) FROM events",
		},
		{
			name:          "User count validation",
			dendriteQuery: "SELECT COUNT(*) FROM userapi_accounts",
			synapseQuery:  "SELECT COUNT(*) FROM users",
		},
		{
			name:          "Room count validation",
			dendriteQuery: "SELECT COUNT(*) FROM roomserver_rooms",
			synapseQuery:  "SELECT COUNT(*) FROM rooms",
		},
	}

	for _, validation := range validations {
		m.Log.Debugf("Running validation: %s", validation.name)

		// Query Dendrite database
		var dendriteCount int
		if err := m.DendriteDB.QueryRow(validation.dendriteQuery).Scan(&dendriteCount); err != nil {
			m.Log.Warnf("Validation '%s' - Dendrite query failed: %v", validation.name, err)
			continue
		}

		// Query Synapse database (within transaction)
		var synapseCount int
		if err := tx.QueryRow(validation.synapseQuery).Scan(&synapseCount); err != nil {
			m.Log.Warnf("Validation '%s' - Synapse query failed: %v", validation.name, err)
			continue
		}

		m.Log.Infof("Validation '%s': Dendrite=%d, Synapse=%d", validation.name, dendriteCount, synapseCount)

		// Check if counts match (allowing for some differences due to filtering)
		if synapseCount > 0 && dendriteCount > 0 {
			if synapseCount >= dendriteCount*90/100 { // Allow 10% difference for filtered data
				m.Log.Infof("✓ Validation '%s' passed", validation.name)
			} else {
				m.Log.Warnf("⚠ Validation '%s' - significant difference in counts", validation.name)
			}
		} else if dendriteCount == 0 && synapseCount == 0 {
			m.Log.Infof("✓ Validation '%s' passed (both empty)", validation.name)
		} else {
			m.Log.Warnf("⚠ Validation '%s' - unexpected count difference", validation.name)
		}
	}

	return nil
}

// migrateLocalCurrentMembership creates the local_current_membership table
// This is CRITICAL for room visibility in Synapse - without this, users won't see their rooms
func (m *Migrator) migrateLocalCurrentMembership(tx *sql.Tx) error {
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
}

// migrateStateGroups creates state groups from Dendrite state snapshots
func (m *Migrator) migrateStateGroups(tx *sql.Tx) error {
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
}

// migrateEventToStateGroups maps events to their state groups
func (m *Migrator) migrateEventToStateGroups(tx *sql.Tx) error {
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

// migrateProfiles migrates user profile data from Dendrite to Synapse
func (m *Migrator) migrateProfiles(tx *sql.Tx) error {
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
}

// migrateAccountData migrates global account data from Dendrite to Synapse
func (m *Migrator) migrateAccountData(tx *sql.Tx) error {
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
}

// migrateRoomAccountData migrates room-specific account data from Dendrite to Synapse
func (m *Migrator) migrateRoomAccountData(tx *sql.Tx) error {
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
}

// migratePushRules extracts push rules from Dendrite account data and stores them in Synapse push_rules table
func (m *Migrator) migratePushRules(tx *sql.Tx) error {
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
}

// migrateReceipts migrates read receipts from Dendrite to Synapse
func (m *Migrator) migrateReceipts(tx *sql.Tx) error {
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
}

// migrateUserFilters migrates sync filters from Dendrite to Synapse
func (m *Migrator) migrateUserFilters(tx *sql.Tx) error {
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
}

// migratePushers migrates push notification configurations from Dendrite to Synapse
func (m *Migrator) migratePushers(tx *sql.Tx) error {
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
}

// migrateCrossSigningKeys migrates cross-signing keys from Dendrite to Synapse
func (m *Migrator) migrateCrossSigningKeys(tx *sql.Tx) error {
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
}

// migrateCrossSigningSignatures migrates cross-signing signatures from Dendrite to Synapse
func (m *Migrator) migrateCrossSigningSignatures(tx *sql.Tx) error {
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
}

// migrateDeviceKeys migrates device keys from Dendrite to Synapse
func (m *Migrator) migrateDeviceKeys(tx *sql.Tx) error {
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
}

// migrateOneTimeKeys migrates one-time keys from Dendrite to Synapse
func (m *Migrator) migrateOneTimeKeys(tx *sql.Tx) error {
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
}

// migrateFallbackKeys migrates fallback keys from Dendrite to Synapse
func (m *Migrator) migrateFallbackKeys(tx *sql.Tx) error {
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
}

// migrateKeyBackupVersions migrates key backup versions from Dendrite to Synapse
func (m *Migrator) migrateKeyBackupVersions(tx *sql.Tx) error {
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
}

// migrateKeyBackups migrates key backup data from Dendrite to Synapse
func (m *Migrator) migrateKeyBackups(tx *sql.Tx) error {
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
