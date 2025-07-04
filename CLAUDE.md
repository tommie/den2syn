# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

### Building and Running
```bash
# Build the migration tool
go build -o den2syn ./cmd/den2syn

# Run in dry-run mode (default)
go run ./cmd/den2syn pg://local/dendrite-prod pg://local/synapse

# Run with actual migration (commits changes)
go run ./cmd/den2syn --commit pg://local/dendrite-prod pg://local/synapse

# Run with verbose logging
go run ./cmd/den2syn --verbose pg://local/dendrite-prod pg://local/synapse
```

### Testing
```bash
# Run tests
go test ./...

# Run tests with coverage
go test -cover ./...

# Run tests in verbose mode
go test -v ./...
```

### Development
```bash
# Format code
go fmt ./...

# Run static analysis
go vet ./...

# Tidy modules
go mod tidy

# Download dependencies
go mod download
```

## Architecture

### High-Level Structure
- **Main Tool**: `cmd/den2syn/` - Command-line interface for the migration tool
- **Migration Engine**: Core logic for database migration from Dendrite to Synapse
- **Reference Sources**:
  - `sources/dendrite/` - Full Dendrite Matrix homeserver codebase (Go)
  - `sources/synapse/` - Full Synapse Matrix homeserver codebase (Python)

### Key Components
- **Database Migration**: Handles PostgreSQL database schema and data migration
- **Idempotent Operations**: Designed to run multiple times safely without data corruption
- **Dry-run Mode**: Default mode that shows what would be done without making changes
- **Progress Tracking**: Analyzes and explains migration steps before execution

### Database Architecture
The tool migrates between two different Matrix homeserver implementations:
- **Dendrite** (Go-based): Source database with Go-specific schema and data structures
- **Synapse** (Python-based): Target database with Python-specific schema and data structures

### Migration Strategy
- Uses embedded source codebases to understand both database schemas
- Implements idempotent migration logic to handle partial migrations
- Supports PostgreSQL connections with standard connection strings
- Provides detailed progress reporting and logging

## Development Notes

### Module Structure
- Go module: `github.com/tommie/den2syn`
- Go version: 1.24.4
- Standard Go project layout with `cmd/` directory for applications

### Key Dependencies
The tool will need to be compatible with:
- Dendrite's Go codebase for understanding source schema
- Synapse's Python codebase for understanding target schema
- PostgreSQL drivers for database operations
- Command-line argument parsing libraries

### Implementation Status
The project is in early development phase:
- Basic project structure established
- Main application entry point needs to be implemented
- Migration logic needs to be developed
- Database schema analysis needs to be implemented

## Database Schema Analysis

### Dendrite Database Structure (v0.11.1)

Dendrite uses a microservice architecture with separate storage components:

#### Core Tables and Locations

**RoomServer** (`sources/dendrite/roomserver/storage/postgres/`):
- `roomserver_rooms` - Room metadata and state snapshots
  - Schema: `postgres/rooms_table.go:32-49`
  - Key fields: `room_nid`, `room_id`, `latest_event_nids`, `state_snapshot_nid`, `room_version`

- `roomserver_events` - Event metadata without content
  - Schema: `postgres/events_table.go:37-71`
  - Key fields: `event_nid`, `room_nid`, `event_id`, `event_type_nid`, `depth`, `auth_event_nids`

- `roomserver_event_json` - Event content (JSON)
  - Schema: `postgres/event_json_table.go:31-42`
  - Key fields: `event_nid`, `event_json`

- `roomserver_membership` - User-room membership states
  - Schema: `postgres/membership_table.go:40-67`
  - Key fields: `room_nid`, `target_nid`, `membership_nid`, `event_nid`

**UserAPI** (`sources/dendrite/userapi/storage/postgres/`):
- `userapi_accounts` - User account data
  - Schema: `postgres/accounts_table.go:36-52`
  - Key fields: `localpart`, `server_name`, `password_hash`, `created_ts`, `account_type`

- `userapi_devices` - Device sessions and access tokens
  - Schema: `postgres/devices_table.go:38-67`
  - Key fields: `access_token`, `device_id`, `localpart`, `server_name`, `created_ts`

**SyncAPI** (`sources/dendrite/syncapi/storage/postgres/`):
- `syncapi_output_room_events` - Event timeline for /sync
  - Schema: `postgres/output_room_events_table.go:40-73`
  - Key fields: `id`, `event_id`, `room_id`, `headered_event_json`, `type`, `sender`

**FederationAPI** (`sources/dendrite/federationapi/storage/postgres/`):
- `federationsender_queue_pdus` - Outbound federation queue
  - Schema: `postgres/queue_pdus_table.go:28-35`
  - Key fields: `transaction_id`, `server_name`, `json_nid`

#### Key Database Patterns

1. **Numeric IDs (NIDs)**: Dendrite uses numeric IDs for performance with lookup tables
2. **Event Storage Split**: Events split between metadata (`roomserver_events`) and content (`roomserver_event_json`)
3. **State Snapshots**: Complex state resolution using snapshot system
4. **Microservice Separation**: Data separated by functional component

#### Storage Interface Files
- **RoomServer Interface**: `roomserver/storage/interface.go:27-185`
- **UserAPI Interface**: `userapi/storage/interface.go:31-155`
- **SyncAPI Interface**: `syncapi/storage/interface.go:30-201`

#### Migration Files
Schema evolution tracked in:
- `userapi/storage/postgres/deltas/` - Account/device migrations
- `roomserver/storage/postgres/deltas/` - Room/event migrations
- `syncapi/storage/postgres/deltas/` - Sync migrations

### Critical Migration Data Flow

1. **Events**: `roomserver_events` + `roomserver_event_json` → All Matrix events
2. **Rooms**: `roomserver_rooms` → Room metadata and current state
3. **Users**: `userapi_accounts` + `userapi_devices` → User data and sessions
4. **Membership**: `roomserver_membership` → User-room relationships
5. **Timeline**: `syncapi_output_room_events` → Event ordering for clients

### Synapse Database Structure (v1.133.0, Schema v72)

Synapse uses a two-database logical structure with comprehensive state management:

#### Core Tables and Schema Locations

**Main Database** (`sources/synapse/synapse/storage/schema/main/full_schemas/72/full.sql.postgres`):

**Event Storage:**
- `events` - Core event table with ordering and metadata
  - Schema: `full.sql.postgres:409-427`
  - Key fields: `topological_ordering`, `stream_ordering`, `event_id`, `type`, `room_id`, `content`, `sender`

- `event_json` - Full event JSON storage
  - Schema: `full.sql.postgres:319-325`
  - Key fields: `event_id`, `room_id`, `internal_metadata`, `json`, `format_version`

- `event_edges` - Event DAG relationships
  - Schema: `full.sql.postgres:305-310`
  - Key fields: `event_id`, `prev_event_id`, `room_id`, `is_state`

- `event_auth` - Event authorization chains
  - Schema: `full.sql.postgres:273-277`
  - Key fields: `event_id`, `auth_id`, `room_id`

**Room Management:**
- `rooms` - Room metadata
  - Schema: `full.sql.postgres:784-790`
  - Key fields: `room_id`, `is_public`, `creator`, `room_version`, `has_auth_chain_index`

- `room_memberships` - Membership events
  - Schema: `full.sql.postgres:728-737`
  - Key fields: `event_id`, `user_id`, `sender`, `room_id`, `membership`, `display_name`, `avatar_url`

- `local_current_membership` - Fast membership lookup
  - Schema: `full.sql.postgres:489-494`
  - Key fields: `room_id`, `user_id`, `event_id`, `membership`

**User Management:**
- `users` - User accounts
  - Schema: `full.sql.postgres:942-956`
  - Key fields: `name`, `password_hash`, `creation_ts`, `admin`, `is_guest`, `deactivated`, `user_type`

- `access_tokens` - Authentication tokens
  - Schema: `full.sql.postgres:15-25`
  - Key fields: `id`, `user_id`, `device_id`, `token`, `valid_until_ms`, `refresh_token_id`

- `devices` - User devices
  - Schema: `full.sql.postgres:208-216`
  - Key fields: `user_id`, `device_id`, `display_name`, `last_seen`, `ip`, `user_agent`

**State Database** (`sources/synapse/synapse/storage/schema/state/full_schemas/72/full.sql.postgres`):

**State Management:**
- `state_groups` - State group definitions
  - Schema: `full.sql.postgres:12-15`
  - Key fields: `id`, `room_id`, `event_id`

- `state_groups_state` - State group contents
  - Schema: `full.sql.postgres:16-22`
  - Key fields: `state_group`, `room_id`, `type`, `state_key`, `event_id`

- `current_state_events` - Current room state
  - Schema: `full.sql.postgres:106-112`
  - Key fields: `event_id`, `room_id`, `type`, `state_key`, `membership`

#### Key Synapse Design Principles

1. **Dual Ordering**: Events use both `topological_ordering` (DAG) and `stream_ordering` (chronological)
2. **State Groups**: Sophisticated state management system for efficient state storage/retrieval
3. **Sharding Support**: Many tables include `instance_name` for multi-worker deployments
4. **Streaming Architecture**: Comprehensive streaming with sequence generators and stream positions
5. **Event Relations**: Full support for threading, reactions, and relations via `event_relations` table

#### Migration Mapping Strategy

**Dendrite → Synapse Table Mapping:**
- `roomserver_events` + `roomserver_event_json` → `events` + `event_json`
- `roomserver_rooms` → `rooms` + state group system
- `roomserver_membership` → `room_memberships` + `local_current_membership`
- `userapi_accounts` → `users` (with additional fields)
- `userapi_devices` → `devices` + `access_tokens`
- `syncapi_output_room_events` → `events` (with proper ordering)

#### Schema Evolution Tracking
- **Delta Files**: `sources/synapse/synapse/storage/schema/main/delta/` (versions 88-92)
- **Recent Features**: Delayed events, user reports, sliding sync, media authentication
- **State Schema**: `sources/synapse/synapse/storage/schema/state/delta/`

### Migration Challenges and Considerations

1. **State Management**: Dendrite's state snapshots need conversion to Synapse's state group system
2. **Event Ordering**: Synapse requires both topological and stream ordering for events
3. **Numeric IDs**: Dendrite's NID system needs mapping to Synapse's string-based primary keys
4. **Microservice Data**: Dendrite's separated storage needs consolidation into Synapse's unified schema
5. **Schema Version**: Target Synapse schema version 72 with latest features

## Migration Implementation Plan

### Required User-Defined Functions (UDFs)

Implemented in Go or SQL.

**UDF 1: `dendrite_nid_to_string(nid BIGINT, table_name TEXT) RETURNS TEXT`**
- Converts Dendrite numeric IDs to string equivalents via lookup tables
- Used for resolving room_nid → room_id, event_nid → event_id, etc.

**UDF 2: `calculate_topological_ordering(event_id TEXT, room_id TEXT) RETURNS BIGINT`**
- Calculates topological ordering based on event DAG position
- Ensures events respect causal ordering in rooms

**UDF 3: `extract_event_metadata(event_json TEXT) RETURNS TABLE(...)`**
- Extracts structured metadata from Dendrite event JSON
- Returns sender, origin_server_ts, content, state_key, etc.

**UDF 4: `build_state_group(room_id TEXT, event_id TEXT) RETURNS BIGINT`**
- Creates or finds appropriate state groups for room states
- Implements Synapse's state group deduplication algorithm

**UDF 5: `convert_auth_events(auth_event_nids BIGINT[]) RETURNS TEXT[]`**
- Converts Dendrite auth event NID arrays to Synapse event ID arrays
- Maps authorization chains between systems

### Migration Execution Phases

**Phase 1: Preparation and Validation**
- Verify schema versions and data integrity
- Check for orphaned events, duplicate IDs, invalid rooms

**Phase 2: NID Resolution System**
- Create temporary mapping tables for all Dendrite NIDs
- Populate room_nid → room_id, event_nid → event_id mappings
- Build user_nid, event_type_nid, state_key_nid mappings

**Phase 3: Core Data Migration**
- Migrate users: userapi_accounts → users, userapi_devices → devices + access_tokens
- Migrate rooms: roomserver_rooms → rooms
- Migrate events: roomserver_events + roomserver_event_json → events + event_json
- Migrate relationships: auth_event_nids → event_auth, prev_events → event_edges

**Phase 4: State and Membership Migration**
- Convert state snapshots to state groups: state_snapshot_nid → state_groups
- Build current state: roomserver_state_block → current_state_events
- Migrate membership: roomserver_membership → room_memberships + local_current_membership

**Phase 5: Advanced Features**
- Migrate account data: userapi_account_data → account_data + room_account_data
- Migrate push rules: userapi_pushrules → push_rules
- Update stream positions and sequence generators

### Critical Migration Mappings

**Users and Authentication:**
- `userapi_accounts` → `users` (with account_type → admin/is_guest mapping)
- `userapi_devices` → `devices` + `access_tokens` (split authentication data)

**Events and Content:**
- `roomserver_events` + `roomserver_event_json` → `events` + `event_json`
- Event ordering: depth → topological_ordering, sequential → stream_ordering
- Auth chains: auth_event_nids[] → event_auth table

**State Management:**
- `roomserver_state_snapshots` → `state_groups` (direct NID mapping)
- `roomserver_state_block` → `state_groups_state` (state group contents)
- Latest room state → `current_state_events` (fast state lookup)

**Membership:**
- `roomserver_membership` → `room_memberships` (full membership events)
- Local users → `local_current_membership` (fast membership lookup)

### Transaction Safety and Verification

**Execution Order:**
1. All migrations run in single transaction for atomicity
2. Users → Rooms → Events → State → Membership → Advanced features
3. Foreign key dependencies respected throughout

**Post-Migration Verification:**
- Verify record counts match between Dendrite and Synapse
- Check for missing event JSON, orphaned references
- Validate state consistency and membership integrity

**Rollback Strategy:**
- Single transaction allows complete rollback on any failure
- Temporary mapping tables enable rerun without data corruption
- Idempotent design supports partial migration retry
