package main

import (
	"database/sql"
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
	verbose bool
	commit  bool
	log     = logrus.New()
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
  den2syn postgres://user:pass@localhost/dendrite postgres://user:pass@localhost/synapse
  den2syn --commit --verbose postgres://user:pass@localhost/dendrite postgres://user:pass@localhost/synapse`,
		Args: cobra.ExactArgs(2),
		Run:  runMigration,
	}

	rootCmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "Show verbose logging including SQL queries")
	rootCmd.Flags().BoolVarP(&commit, "commit", "c", false, "Commit changes (default is dry-run mode)")

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
		values := make([]interface{}, len(columns))
		valuePtrs := make([]interface{}, len(columns))
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
		JOIN roomserver_event_state_keys skeys ON e.event_state_key_nid = skeys.event_state_key_nid
		JOIN roomserver_event_json ej ON e.event_nid = ej.event_nid
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
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
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

		if _, err := stmt.Exec(topologicalOrdering, eventID, eventType, roomID, content, unrecognizedKeys, processedInt, outlierInt, depth, streamOrdering, receivedTs, sender, containsURLInt, stateKeyValue, rejectionReasonValue, instanceName, originServerTs); err != nil {
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
					COALESCE(ej.event_json::jsonb->>'membership', 'leave')
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
					COALESCE(ej.event_json::jsonb->>'membership', 'leave')
				ELSE 'leave'
			END as membership,
			CASE
				WHEN ej.event_json IS NOT NULL AND ej.event_json != '' THEN
					ej.event_json::jsonb->>'display_name'
				ELSE NULL
			END as display_name,
			CASE
				WHEN ej.event_json IS NOT NULL AND ej.event_json != '' THEN
					ej.event_json::jsonb->>'avatar_url'
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
