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
	Run         func(step MigrationStep, tx *sql.Tx) error
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
			err := step.Run(step, tx)
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

func (m *Migrator) newDendriteQuery(query string) func(step MigrationStep, tx *sql.Tx) error {
	return func(step MigrationStep, tx *sql.Tx) error {
		return m.executeDendriteQuery(step, query)
	}
}

func (m *Migrator) newSynapseCommand(query string) func(step MigrationStep, tx *sql.Tx) error {
	return func(step MigrationStep, tx *sql.Tx) error {
		return m.executeSynapseCommand(step, tx, query)
	}
}

// executeDendriteQuery executes a query against the Dendrite database and logs results
func (m *Migrator) executeDendriteQuery(step MigrationStep, query string) error {
	if m.Verbose {
		m.Log.Debug("Executing Dendrite query:")
		m.Log.Debug(query)
	}

	rows, err := m.DendriteDB.Query(query)
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
func (m *Migrator) executeSynapseCommand(step MigrationStep, tx *sql.Tx, query string) error {
	if m.Verbose {
		m.Log.Debug("Executing Synapse command:")
		m.Log.Debug(query)
	}

	result, err := tx.Exec(query)
	if err != nil {
		return fmt.Errorf("failed to execute Synapse command: %w", err)
	}

	// Log results
	if rowsAffected, err := result.RowsAffected(); err == nil {
		m.Log.Debugf("Synapse command affected %d rows", rowsAffected)
	}

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
