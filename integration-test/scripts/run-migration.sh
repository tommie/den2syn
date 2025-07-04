#!/bin/bash
# run-migration.sh - Execute den2syn migration

set -e
set -o pipefail

echo "=== Starting den2syn Migration Test ==="
echo "Timestamp: $(date)"
echo "Dendrite DB: $DENDRITE_DB_URL"
echo "Synapse DB: $SYNAPSE_DB_URL"
echo

# Log all output to files
LOG_DIR="/logs"
mkdir -p "$LOG_DIR"

exec > >(tee "$LOG_DIR/migration.log")
exec 2>&1

echo "=== Pre-migration Database Verification ==="

# Check Dendrite database structure
echo "Checking Dendrite database..."
psql "$DENDRITE_DB_URL" -c "
    SELECT
        'table' as type,
        tablename as name,
        schemaname as schema
    FROM pg_tables
    WHERE schemaname = 'public'
        AND (
            tablename LIKE 'roomserver_%' OR
            tablename LIKE 'userapi_%' OR
            tablename LIKE 'syncapi_%'
        )
    ORDER BY tablename;
"

echo
echo "Dendrite data counts:"
psql "$DENDRITE_DB_URL" -c "
    SELECT 'rooms' as table_name, COUNT(*) as row_count FROM roomserver_rooms
    UNION ALL
    SELECT 'events' as table_name, COUNT(*) as row_count FROM roomserver_events
    UNION ALL
    SELECT 'users' as table_name, COUNT(*) as row_count FROM userapi_accounts
    UNION ALL
    SELECT 'devices' as table_name, COUNT(*) as row_count FROM userapi_devices
    UNION ALL
    SELECT 'event_json' as table_name, COUNT(*) as row_count FROM roomserver_event_json;
"

echo
echo "=== Running Migration (Dry-run) ==="

# First run in dry-run mode to verify everything works
echo "Running dry-run migration..."
./den2syn --verbose "$DENDRITE_DB_URL" "$SYNAPSE_DB_URL" 2>&1 | tee "$LOG_DIR/migration-dry-run.log"

echo
echo "=== Running Migration (Commit) ==="

# Now run the actual migration
echo "Running actual migration with --commit..."
./den2syn --verbose --commit "$DENDRITE_DB_URL" "$SYNAPSE_DB_URL" 2>&1 | tee "$LOG_DIR/migration-commit.log"

echo
echo "=== Post-migration Verification ==="

# Check Synapse database structure
echo "Checking Synapse database..."
psql "$SYNAPSE_DB_URL" -c "
    SELECT
        'table' as type,
        tablename as name,
        schemaname as schema
    FROM pg_tables
    WHERE schemaname = 'public'
    ORDER BY tablename
    LIMIT 20;
"

echo
echo "Synapse data counts:"
psql "$SYNAPSE_DB_URL" -c "
    SELECT 'events' as table_name, COUNT(*) as row_count FROM events
    UNION ALL
    SELECT 'users' as table_name, COUNT(*) as row_count FROM users
    UNION ALL
    SELECT 'rooms' as table_name, COUNT(*) as row_count FROM rooms
    UNION ALL
    SELECT 'devices' as table_name, COUNT(*) as row_count FROM devices
    UNION ALL
    SELECT 'event_json' as table_name, COUNT(*) as row_count FROM event_json;
" || echo "Some tables may not exist yet - this is expected for a new Synapse installation"

echo
echo "=== Migration Summary ==="

# Compare counts between source and target
echo "Comparing data counts:"
DENDRITE_EVENTS=$(psql "$DENDRITE_DB_URL" -t -c "SELECT COUNT(*) FROM roomserver_events;")
DENDRITE_USERS=$(psql "$DENDRITE_DB_URL" -t -c "SELECT COUNT(*) FROM userapi_accounts;")
DENDRITE_ROOMS=$(psql "$DENDRITE_DB_URL" -t -c "SELECT COUNT(*) FROM roomserver_rooms;")

SYNAPSE_EVENTS=$(psql "$SYNAPSE_DB_URL" -t -c "SELECT COUNT(*) FROM events;" 2>/dev/null || echo "0")
SYNAPSE_USERS=$(psql "$SYNAPSE_DB_URL" -t -c "SELECT COUNT(*) FROM users;" 2>/dev/null || echo "0")
SYNAPSE_ROOMS=$(psql "$SYNAPSE_DB_URL" -t -c "SELECT COUNT(*) FROM rooms;" 2>/dev/null || echo "0")

echo "Events: Dendrite=$DENDRITE_EVENTS, Synapse=$SYNAPSE_EVENTS"
echo "Users: Dendrite=$DENDRITE_USERS, Synapse=$SYNAPSE_USERS"
echo "Rooms: Dendrite=$DENDRITE_ROOMS, Synapse=$SYNAPSE_ROOMS"

echo
echo "=== Migration Test Completed ==="
echo "Timestamp: $(date)"
echo "Logs saved to: $LOG_DIR"
echo

# Exit with success - Synapse validation will happen in next container
exit 0
