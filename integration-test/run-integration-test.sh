#!/bin/bash
# run-integration-test.sh - Main integration test runner

set -e
set -o pipefail

echo "=== den2syn Integration Test Suite ==="
echo "Starting integration test at $(date)"
echo

# Change to integration test directory
cd "$(dirname "$0")"

echo "Building all containers..."
docker compose build

# Clean up any existing containers and volumes
echo "Cleaning up previous test runs..."
docker compose down --volumes --remove-orphans 2>/dev/null || true

echo
echo "=== Building and Starting Test Environment ==="

# Start the complete test environment
echo "Starting PostgreSQL and loading Dendrite data..."
docker compose up -d postgres

# Wait for PostgreSQL to be healthy
echo "Waiting for PostgreSQL to initialize..."
timeout 30 bash -c 'until docker compose ps postgres | grep -qF "(healthy)"; do sleep 2 || exit 1 ; done'

echo
echo "PostgreSQL is ready. Database structure:"
docker compose exec postgres psql -U matrix -d dendrite -c "
    SELECT
        tablename,
        schemaname
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
echo "Sample data counts in Dendrite:"
docker compose exec postgres psql -U matrix -d dendrite -c "
    SELECT 'rooms' as table_name, COUNT(*) as row_count FROM roomserver_rooms
    UNION ALL
    SELECT 'events' as table_name, COUNT(*) as row_count FROM roomserver_events
    UNION ALL
    SELECT 'users' as table_name, COUNT(*) as row_count FROM userapi_accounts
    UNION ALL
    SELECT 'devices' as table_name, COUNT(*) as row_count FROM userapi_devices;
"

echo
echo "=== Initializing Synapse Database Schema ==="

# Initialize Synapse database schema first
echo "Initializing Synapse database schema..."
if docker compose up --exit-code-from synapse-schema-init synapse-schema-init; then
    echo "✓ Synapse database schema initialized successfully"
else
    echo "✗ Synapse database schema initialization failed"
    echo "Schema initialization logs:"
    docker compose logs synapse-schema-init
    exit 1
fi

echo
echo "=== Running Migration ==="

# Run the migration
echo "Starting den2syn migration..."
if docker compose up --exit-code-from den2syn-migration den2syn-migration; then
    echo "✓ Migration completed successfully"
else
    echo "✗ Migration failed"
    echo "Migration logs:"
    docker compose logs den2syn-migration
    exit 1
fi

echo
echo "=== Starting Synapse Validation ==="

# Start Synapse for validation
echo "Starting Synapse..."
docker compose up -d synapse

# Wait for Synapse to be healthy
echo "Waiting for Synapse to start..."
timeout 30 bash -c 'until docker compose ps synapse | grep -qF "(healthy)"; do sleep 2 || exit 1 ; done'

echo
echo "=== Running Integration Tests ==="

# Run integration tests
if docker compose up --exit-code-from integration-test integration-test; then
    echo "✓ Integration tests passed"
    TEST_STATUS="PASSED"
else
    echo "✗ Integration tests failed"
    echo "Test logs:"
    docker compose logs integration-test
    TEST_STATUS="FAILED"
    exit 1
fi

echo
echo "=== Test Results ==="

# Show migration logs
echo "Migration summary:"
docker compose exec den2syn-migration cat /logs/migration.log | tail -20 || true

# Show test results
echo
echo "Integration test summary:"
if docker compose exec integration-test test -f /test-results/test-summary.json; then
    docker compose exec integration-test cat /test-results/test-summary.json | jq . 2>/dev/null || cat
else
    echo "Test summary not available"
fi

echo
echo "=== Final Verification ==="

# Final database verification
echo "Final data verification:"
docker compose exec postgres psql -U matrix -d synapse -c "
    SELECT 'events' as table_name, COUNT(*) as row_count FROM events
    UNION ALL
    SELECT 'users' as table_name, COUNT(*) as row_count FROM users
    UNION ALL
    SELECT 'rooms' as table_name, COUNT(*) as row_count FROM rooms
    UNION ALL
    SELECT 'devices' as table_name, COUNT(*) as row_count FROM devices;
" 2>/dev/null || echo "Some tables may not be accessible"

echo
echo "=== Cleanup ==="

# Save logs before cleanup
echo "Saving logs..."
mkdir -p ./test-results
docker compose logs > ./test-results/docker-compose.log 2>&1

# Optional: Keep environment running for manual inspection
if [ "x${CI:-}" = true ]; then
    REPLY=n
else
    read -p "Keep test environment running for manual inspection? (y/N): " -n 1 -r
    echo
fi
if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo "Cleaning up test environment..."
    docker compose down --volumes
else
    echo "Test environment kept running. Use 'docker compose down --volumes' to clean up."
    echo "Access Synapse at: http://localhost:8008"
    echo "Access PostgreSQL: docker compose exec postgres psql -U matrix -d synapse"
fi

echo
echo "=== Integration Test Complete ==="
echo "Status: $TEST_STATUS"
echo "Completed at: $(date)"

if [ "$TEST_STATUS" = "PASSED" ]; then
    exit 0
else
    exit 1
fi
