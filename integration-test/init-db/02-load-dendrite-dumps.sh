#!/bin/bash
set -e

echo "Loading Dendrite dump file..."

# Check if dump file exists
if [ ! -f "/dendrite-dumps/dendrite.dump" ]; then
    echo "ERROR: dendrite.dump file not found at /dendrite-dumps/dendrite.dump"
    ls -la /dendrite-dumps/
    exit 1
fi

echo "Found dendrite.dump file, checking size..."
ls -lh /dendrite-dumps/dendrite.dump

# Load the consolidated Dendrite dump
echo "Loading dendrite.dump into dendrite database..."

# Check if we can connect to the database first
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "dendrite" -c "SELECT 'Database connection successful' as status;"

# Load the dump file directly
echo "Starting dump restore..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "dendrite" < "/dendrite-dumps/dendrite.dump"

echo "Dump restore completed, checking results..."

# Verify the database has the expected tables
echo "Verifying Dendrite database structure..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "dendrite" <<-EOSQL
    -- Check for required tables
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
    
    -- Count some key data
    SELECT 'rooms' as table_name, COUNT(*) as row_count FROM roomserver_rooms
    UNION ALL
    SELECT 'events' as table_name, COUNT(*) as row_count FROM roomserver_events
    UNION ALL
    SELECT 'users' as table_name, COUNT(*) as row_count FROM userapi_accounts
    UNION ALL
    SELECT 'devices' as table_name, COUNT(*) as row_count FROM userapi_devices;
EOSQL

# Initialize Synapse database - just ensure it's ready
echo "Preparing Synapse database..."
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "synapse" <<-EOSQL
    -- Synapse will create its own schema during first run
    SELECT 'Synapse database ready for migration' as status;
EOSQL

echo "Database initialization completed successfully!"