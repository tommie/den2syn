# den2syn - Matrix Dendrite to Synapse Migration Tool

A tool for migrating Matrix homeserver data from Dendrite to Synapse, implemented in Go.

## Features

- **Idempotent Migration**: Can run multiple times without destroying data
- **Progress Tracking**: Visual progress bars for each migration phase
- **Dry-run Mode**: Default safe mode that shows what would be done without making changes
- **Verbose Logging**: Detailed SQL query logging with `--verbose` flag
- **Transaction Safety**: All operations in a single transaction with rollback support
- **PostgreSQL Support**: Optimized for PostgreSQL databases

## Installation

```bash
# Clone the repository
git clone https://github.com/tommie/den2syn
cd den2syn

# Build the tool
go build -o den2syn ./cmd/den2syn

# Or run directly
go run ./cmd/den2syn [flags] <dendrite-db-url> <synapse-db-url>
```

## Usage

### Basic Usage

```bash
# Dry-run mode (safe, shows what would be done)
./den2syn postgres://user:pass@localhost/dendrite postgres://user:pass@localhost/synapse

# Actually perform the migration
./den2syn --commit postgres://user:pass@localhost/dendrite postgres://user:pass@localhost/synapse

# Verbose mode (shows SQL queries)
./den2syn --verbose --commit postgres://user:pass@localhost/dendrite postgres://user:pass@localhost/synapse
```

### Database URL Format

Database URLs should be in PostgreSQL format:
- `postgresql://user:password@host:port/database`
- `postgres://user:password@host:port/database`

### Command Line Options

- `-c, --commit`: Commit changes (default is dry-run mode)
- `-v, --verbose`: Show verbose logging including SQL queries
- `-h, --help`: Show help information

## Migration Process

The tool performs migration in 5 phases:

### Phase 1: Preparation and Validation
- Validates database schemas and connectivity
- Checks data integrity in source database
- Verifies required tables exist

### Phase 2: NID Resolution System
- Creates temporary mapping tables for Dendrite's numeric IDs
- Populates room, event, user, and type mappings
- Establishes relationship lookups

### Phase 3: Core Data Migration
- Migrates user accounts and authentication data
- Migrates device information and access tokens
- Migrates rooms and room metadata
- Migrates all events with proper JSON content
- Migrates event relationships and authorization chains

### Phase 4: State and Membership Migration
- Converts Dendrite's state snapshots to Synapse's state groups
- Migrates current room state for fast lookups
- Migrates room membership data with history

### Phase 5: Advanced Features
- Updates sequence generators and stream positions
- Migrates account data and user preferences
- Migrates push notification rules
- Finalizes migration with integrity checks

## Implementation Details

### Database Schema Compatibility

The tool migrates from:
- **Dendrite v0.11.1** database schema
- **Synapse v1.133.0** database schema (Schema v72)

### Safety Features

- **Atomic Transactions**: All operations in single transaction
- **Dry-run Default**: No changes unless `--commit` specified
- **Data Validation**: Pre and post-migration integrity checks
- **Rollback Support**: Complete rollback on any failure
- **Idempotent Design**: Safe to run multiple times

## Prerequisites

- Go 1.24.4 or later
- PostgreSQL databases for both Dendrite and Synapse
- Sufficient database permissions for schema modifications
- Backup of source database recommended

## Example Output

```
$ ./den2syn --verbose postgres://user:pass@localhost/dendrite postgres://user:pass@localhost/synapse

time="2025-07-04T09:23:20+02:00" level=debug msg="Verbose logging enabled"
time="2025-07-04T09:23:20+02:00" level=info msg="Starting den2syn migration" commit_mode=false
time="2025-07-04T09:23:20+02:00" level=info msg="Connecting to databases..."
time="2025-07-04T09:23:20+02:00" level=info msg="Phase: Phase 1 - Preparation and Validation"
Migration Progress [==============>                 ] 45% | 12/27 steps
time="2025-07-04T09:23:25+02:00" level=info msg="Phase: Phase 2 - NID Resolution System"
Migration Progress [=========================>      ] 78% | 21/27 steps
time="2025-07-04T09:23:30+02:00" level=info msg="Migration completed successfully!"
```

## Contributing

Contributions are welcome! Please ensure all changes include:

- Appropriate tests
- Documentation updates
- Backward compatibility considerations
