# den2syn Integration Test

This directory contains a complete integration test suite for the den2syn migration tool using Docker Compose.

## Overview

The integration test:

1. **Starts PostgreSQL** and loads real Dendrite data from dump files
2. **Runs den2syn migration** from Dendrite to Synapse format
3. **Starts Synapse** using the migrated database
4. **Validates the migration** with comprehensive tests
5. **Verifies Synapse functionality** with API calls and data integrity checks

## Prerequisites

- Docker and Docker Compose v2
- Real Dendrite database dump file in `dendrite-dumps/` directory
- Dendrite's media folder (`media_api.base_path`) copied to `dendrite-dumps/media/`
- At least 4GB of available RAM for all containers

The dump files are standard PostgreSQL dump files in ASCII text format with CRLF line terminators, containing CREATE TABLE, COPY, and other SQL statements.
It's assumed the database is called `dendrite`, and there should be no `CREATE DATABASE`.

## Quick Start

```bash
export SERVER_NAME=<your-homeserver>

./run-integration-test.sh
```

This will:
- Remove containers and volumes from any previous run
- Build all necessary containers
- Load the Dendrite database from dump files
- Check the table stories, then run the migration
- Start Synapse with the migrated data
- Run some validation tests
- Show detailed results and logs

## Test Architecture

### Services

1. **postgres**: PostgreSQL 17 with Dendrite data loaded
2. **den2syn-migration**: Runs the migration tool in the Synapse image
3. **synapse**: Synapse homeserver using migrated data
4. **integration-test**: Test runner with validation suite

### Volumes

- `postgres_data`: Database storage
- `migration_logs`: Migration execution logs
- `synapse_data`: Synapse storage (configs and media)
- `test_results`: Test results and reports

### Test Flow

```
PostgreSQL ← Load Dendrite dumps
    ↓
den2syn story check (--check-only)
    ↓
den2syn migration
    ↓
Synapse startup with migrated DB
    ↓
Integration tests & validation
    ↓
Results and cleanup
```

### Debug Commands

```bash
# Check database contents
docker compose exec postgres psql -U matrix -d dendrite -c "SELECT COUNT(*) FROM roomserver_events;"
docker compose exec postgres psql -U matrix -d synapse -c "SELECT COUNT(*) FROM events;"

# View migration logs
docker compose exec den2syn-migration cat /logs/migration.log

# Check Synapse status
docker compose exec synapse curl http://localhost:8008/_matrix/static/

# View all logs
docker compose logs
```

### Log Locations

- Migration logs: Container `/logs/` directory
- Synapse logs: Container `/data/homeserver.log`
- Test results: Container `/test-results/` directory
- Docker logs: `./test-results/docker-compose.log`

## Customization

### Different Dump Files

To use different Dendrite dump files:

1. Replace files in `dendrite-dumps/` directory with your dump files
2. Update the `SERVER_NAME` environment variable in `docker-compose.yml` if the domain changes
3. Ensure the dump contains PostgreSQL-compatible schema

## CI/CD Integration

For automated testing:

```bash
# Non-interactive mode
export CI=true
./run-integration-test.sh

# Check exit code
echo $?  # 0 = success, 1 = failure
```

The test suite is designed to be fully automated and suitable for continuous integration environments.
