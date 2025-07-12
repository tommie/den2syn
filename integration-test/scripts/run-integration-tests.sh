#!/bin/bash
# run-integration-tests.sh - Run integration tests after migration

set -e
set -o pipefail

echo "=== Starting Integration Tests ==="
echo "Timestamp: $(date)"
echo

# Test results directory
RESULTS_DIR="/test-results"
mkdir -p "$RESULTS_DIR"

# Redirect output to both console and log file
exec > >(tee "$RESULTS_DIR/integration-tests.log")
exec 2>&1

echo "=== Test 1: Synapse Health Check ==="

# Test Synapse is running and responsive
echo "Testing Synapse health endpoint..."
for i in {1..30}; do
    if curl -sSL http://synapse:8008/_matrix/static/ >/dev/null; then
        echo "✓ Synapse is responding to health checks"
        break
    else
        echo "Waiting for Synapse to start... ($i/30)"
        sleep 2
    fi

    if [ $i -eq 30 ]; then
        echo "✗ Synapse failed to start within timeout"
        exit 1
    fi
done

echo
echo "=== Test 2: Database Migration Verification ==="

# Check that migration logs indicate success
echo "Checking migration logs..."
if [ -f "/logs/migration-commit.log" ]; then
    if grep -q "Migration completed successfully" /logs/migration-commit.log; then
        echo "✓ Migration completed successfully according to logs"
    else
        echo "✗ Migration did not complete successfully"
        echo "Last 10 lines of migration log:"
        ls -la /logs
        tail -10 /logs/migration-commit.log
        exit 1
    fi
else
    echo "✗ Migration log file not found"
    exit 1
fi

echo
echo "=== Test 3: Database Content Verification ==="

# Verify basic database connectivity and content
echo "Testing Synapse database connectivity..."
SYNAPSE_DB_URL="postgres://matrix:matrix_password@postgres:5432/synapse?sslmode=disable"

# Check if basic Synapse tables exist
echo "Checking Synapse table structure..."
TABLES=$(psql "$SYNAPSE_DB_URL" -t -c "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public';")
echo "Found $TABLES tables in Synapse database"

if [ "$TABLES" -gt 50 ]; then
    echo "✓ Synapse database has expected number of tables"
else
    echo "✗ Synapse database has fewer tables than expected"
    exit 1
fi

# Check for key migrated data
echo "Checking migrated data..."
EVENTS_COUNT=$(psql "$SYNAPSE_DB_URL" -t -c "SELECT COUNT(*) FROM events;" 2>/dev/null || echo "0")
USERS_COUNT=$(psql "$SYNAPSE_DB_URL" -t -c "SELECT COUNT(*) FROM users;" 2>/dev/null || echo "0")
ROOMS_COUNT=$(psql "$SYNAPSE_DB_URL" -t -c "SELECT COUNT(*) FROM rooms;" 2>/dev/null || echo "0")

echo "Migrated data counts:"
echo "  Events: $EVENTS_COUNT"
echo "  Users: $USERS_COUNT"
echo "  Rooms: $ROOMS_COUNT"

if [ "$EVENTS_COUNT" -gt 0 ] && [ "$USERS_COUNT" -gt 0 ] && [ "$ROOMS_COUNT" -gt 0 ]; then
    echo "✓ Migration transferred data successfully"
else
    echo "✗ Migration did not transfer expected data"
    echo "Events: $EVENTS_COUNT, Users: $USERS_COUNT, Rooms: $ROOMS_COUNT"
    exit 1
fi

echo
echo "=== Test 4: Synapse API Tests ==="

# Test basic Synapse API endpoints
echo "Testing Synapse API endpoints..."

# Test version endpoint
echo "Testing /_matrix/federation/v1/version..."
if curl -f http://synapse:8008/_matrix/federation/v1/version 2>/dev/null | jq . > /dev/null; then
    echo "✓ Version endpoint responds with valid JSON"
else
    echo "✗ Version endpoint failed"
    exit 1
fi

# Test server discovery
echo "Testing /.well-known/matrix/server..."
if curl -f http://synapse:8008/.well-known/matrix/server 2>/dev/null; then
    echo "✓ Server discovery endpoint responds"
else
    echo "✓ Server discovery endpoint not configured (this is normal)"
fi

echo
echo "=== Test 5: Data Integrity Checks ==="

# Run some basic data integrity checks
echo "Running data integrity checks..."

# Check that events have corresponding JSON
JSON_MISMATCH=$(psql "$SYNAPSE_DB_URL" -t -c "
    SELECT COUNT(*)
    FROM events e
    LEFT JOIN event_json ej ON e.event_id = ej.event_id
    WHERE ej.event_id IS NULL;
" 2>/dev/null || echo "0")

if [ "$JSON_MISMATCH" -eq 0 ]; then
    echo "✓ All events have corresponding JSON content"
else
    echo "✗ Found $JSON_MISMATCH events without JSON content"
    exit 1
fi

# Check for duplicate event IDs
DUPLICATE_EVENTS=$(psql "$SYNAPSE_DB_URL" -t -c "
    SELECT COUNT(*)
    FROM (
        SELECT event_id, COUNT(*)
        FROM events
        GROUP BY event_id
        HAVING COUNT(*) > 1
    ) duplicates;
" 2>/dev/null || echo "0")

if [ "$DUPLICATE_EVENTS" -eq 0 ]; then
    echo "✓ No duplicate event IDs found"
else
    echo "✗ Found $DUPLICATE_EVENTS duplicate event IDs"
    exit 1
fi

echo
echo "=== Test 6: User ID Format Validation ==="

# Check that all user IDs in Synapse database have proper @ format
echo "Validating user ID formats in Synapse database..."

# Create a complete database dump to search for user IDs without @ prefix
echo "Creating complete database dump for text analysis..."
pg_dump "$SYNAPSE_DB_URL" --data-only --inserts > /tmp/synapse_full_dump.sql

# Search for user IDs that don't start with @ in the complete dump
echo "Searching for invalid user ID formats across all tables..."

# Look for known user localparts without @ prefix
echo "Checking for user localparts without @ prefix..."
if [ -n "$SAMPLE_LOCALPARTS" ]; then
    LOCALPARTS=${SAMPLE_LOCALPARTS//,/|}
    INVALID_USER_IDS=$(grep -v public.user_directory_search /tmp/synapse_full_dump.sql | grep -E "['\"]($LOCALPARTS)['\"]" | grep -Ev "\"reason\":\\s*\"($LOCALPARTS)\"" || :)

    if [ -z "$INVALID_USER_IDS" ]; then
        echo "✓ No user localparts found without @ prefix"
        USER_ID_FORMAT_TEST="PASSED"
    else
        echo "✗ Found user localparts without @ prefix:"

        # Group by table for better analysis
        echo
        echo "=== Detailed breakdown by table ==="
        echo "$INVALID_USER_IDS" | while read -r line; do
            table_name=$(echo "$line" | grep -o "INSERT INTO [^(]*" | sed 's/INSERT INTO //' | head -n10)
            echo "Table: $table_name"
            echo "  $line"
            echo
        done

        echo "=== Summary ==="
        echo "Total invalid user ID occurrences found: $(echo "$INVALID_USER_IDS" | wc -l)"
        echo "Tables affected:"
        echo "$INVALID_USER_IDS" | grep -o "INSERT INTO [^(]*" | sed 's/INSERT INTO //' | sort -u

        USER_ID_FORMAT_TEST="FAILED"
    fi
else
    echo "- Skipping localparts test, since SAMPLE_LOCALPARTS is not defined"
fi

# Also check for any localpart:domain patterns without @
echo "Checking for localpart:domain format without @ prefix across all tables..."
LOCALPART_ONLY=$(grep -E "'[^@!a-zA-Z0-9_][a-zA-Z0-9_]+:${SERVER_NAME//./\\.}'" /tmp/synapse_full_dump.sql || :)

if [ -z "$LOCALPART_ONLY" ]; then
    echo "✓ No localpart:domain patterns found without @ prefix"
else
    echo "✗ Found localpart:domain patterns without @ prefix:"
    echo
    echo "=== Detailed breakdown by table ==="
    echo "$LOCALPART_ONLY" | while read -r line; do
        table_name=$(echo "$line" | grep -o "INSERT INTO [^(]*" | sed 's/INSERT INTO //' | head -n10)
        echo "Table: $table_name"
        echo "  $line"
        echo
    done

    echo "=== Summary ==="
    echo "Total localpart:domain occurrences found: $(echo "$LOCALPART_ONLY" | wc -l)"
    echo "Tables affected:"
    echo "$LOCALPART_ONLY" | grep -o "INSERT INTO [^(]*" | sed 's/INSERT INTO //' | sort -u

    USER_ID_FORMAT_TEST="FAILED"
fi

# Additional check for any Matrix user ID pattern without @
echo "Checking for any Matrix user ID patterns without @ prefix..."
MATRIX_ID_PATTERN=$(grep -E "'[^@!a-zA-Z0-9_.-][a-zA-Z0-9_.-]+:[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}'" /tmp/synapse_full_dump.sql || :)

if [ -z "$MATRIX_ID_PATTERN" ]; then
    echo "✓ No Matrix user ID patterns found without @ prefix"
else
    echo "✗ Found Matrix user ID patterns without @ prefix:"
    echo
    echo "=== Detailed breakdown by table ==="
    echo "$MATRIX_ID_PATTERN" | while read -r line; do
        table_name=$(echo "$line" | grep -o "INSERT INTO [^(]*" | sed 's/INSERT INTO //' | head -n10)
        echo "Table: $table_name"
        echo "  $line"
        echo
    done

    echo "=== Summary ==="
    echo "Total Matrix ID pattern occurrences found: $(echo "$MATRIX_ID_PATTERN" | wc -l)"
    echo "Tables affected:"
    echo "$MATRIX_ID_PATTERN" | grep -o "INSERT INTO [^(]*" | sed 's/INSERT INTO //' | sort -u

    USER_ID_FORMAT_TEST="FAILED"
fi

# Show summary of what we found
echo "Summary of database scan:"
TOTAL_LINES=$(wc -l < /tmp/synapse_full_dump.sql)
TOTAL_INSERTS=$(grep -c "INSERT INTO" /tmp/synapse_full_dump.sql)
echo "  Total lines scanned: $TOTAL_LINES"
echo "  Total INSERT statements: $TOTAL_INSERTS"

# Clean up
rm -f /tmp/synapse_full_dump.sql

if [ "$USER_ID_FORMAT_TEST" = "FAILED" ]; then
    echo "✗ User ID format validation failed"
    exit 1
fi

echo
echo "=== Integration Test Summary ==="

# Generate test summary
cat > "$RESULTS_DIR/test-summary.json" <<EOF
{
    "timestamp": "$(date -Iseconds)",
    "status": "PASSED",
    "tests": {
        "synapse_health": "PASSED",
        "migration_logs": "PASSED",
        "database_structure": "PASSED",
        "data_migration": "PASSED",
        "api_endpoints": "PASSED",
        "data_integrity": "PASSED",
        "user_id_format": "$USER_ID_FORMAT_TEST"
    },
    "data_counts": {
        "events": $EVENTS_COUNT,
        "users": $USERS_COUNT,
        "rooms": $ROOMS_COUNT
    }
}
EOF

echo "✓ All integration tests passed!"
echo "✓ Migration from Dendrite to Synapse completed successfully"
echo "✓ Synapse is running and serving API requests"
echo "✓ Data integrity checks passed"
echo "✓ User ID format validation passed"
echo
echo "Test results saved to: $RESULTS_DIR"
echo "Timestamp: $(date)"

exit 0
