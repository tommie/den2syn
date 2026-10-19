#!/bin/bash
# dev-migrate.sh - Runs den2syn against a scratch Synapse database.
#
# Keeps a PostgreSQL container with the Dendrite dump loaded between runs,
# since loading the dump is slow, and recreates the Synapse database every run,
# since den2syn only migrates into a fresh one.
#
# Usage: [DENDRITE_MEDIA=<dir>] scripts/dev-migrate.sh [den2syn args...]
#   e.g. scripts/dev-migrate.sh --check-only
# Without DENDRITE_MEDIA, placeholder media files are generated.

set -euo pipefail

cd "$(dirname "$0")/.."

SYNAPSE_IMAGE=matrixdotorg/synapse:v1.163.0
PG_IMAGE=postgres:17
NETWORK=den2syn-dev
PG=den2syn-dev-pg
DUMP=integration-test/dendrite-dumps/dendrite.dump
DATA=/tmp/den2syn/dev/data
SERVER_NAME=${SERVER_NAME:-gannert.se}

psql_() {
    docker exec -i "$PG" psql -v ON_ERROR_STOP=1 -q -U matrix "$@"
}

docker network inspect "$NETWORK" >/dev/null 2>&1 || docker network create "$NETWORK" >/dev/null

if ! docker container inspect "$PG" >/dev/null 2>&1; then
    echo "Starting PostgreSQL..."
    docker run -d --name "$PG" --network "$NETWORK" \
        -e POSTGRES_USER=matrix -e POSTGRES_PASSWORD=matrix \
        -e POSTGRES_INITDB_ARGS="--encoding=UTF8 --locale=C" \
        "$PG_IMAGE" >/dev/null
fi
docker start "$PG" >/dev/null
until docker exec "$PG" pg_isready -q -U matrix; do sleep 1; done
# pg_isready succeeds during initdb's temporary server, so wait for the real one.
until psql_ -d postgres -c "SELECT 1" >/dev/null 2>&1; do sleep 1; done

if ! psql_ -d postgres -Atc "SELECT 1 FROM pg_database WHERE datname = 'dendrite'" | grep -q 1; then
    echo "Loading the Dendrite dump..."
    # The dump refers to its original owner.
    psql_ -d postgres -c "CREATE ROLE dendrite" -c "CREATE DATABASE dendrite"
    psql_ -d dendrite < "$DUMP"
fi

echo "Recreating the Synapse database..."
psql_ -d postgres -c "DROP DATABASE IF EXISTS synapse" \
    -c "CREATE DATABASE synapse TEMPLATE template0 ENCODING 'UTF8' LOCALE 'C'"

mkdir -p "$DATA"
if [ ! -f "$DATA/homeserver.yaml" ]; then
    echo "Generating Synapse config..."
    docker run --rm --user "$(id -u):$(id -g)" -v "$DATA":/data --entrypoint python "$SYNAPSE_IMAGE" \
        -m synapse.app.homeserver --server-name="$SERVER_NAME" --config-path=/data/homeserver.yaml \
        --generate-config --report-stats=no --data-directory=/data >/dev/null
    docker run --rm --user "$(id -u):$(id -g)" -v "$DATA":/data --entrypoint python "$SYNAPSE_IMAGE" -c "
import yaml
c = yaml.safe_load(open('/data/homeserver.yaml'))
c['database'] = {'name': 'psycopg2', 'args': {'user': 'matrix', 'password': 'matrix', 'host': '$PG', 'database': 'synapse'}}
yaml.safe_dump(c, open('/data/homeserver.yaml', 'w'))
"
    # The generated log config writes to the working directory, which isn't writable.
    sed -i 's|filename: /homeserver.log|filename: /data/homeserver.log|' "$DATA/$SERVER_NAME.log.config"
fi

# Synapse's media store must match the recreated database.
if [ -d "$DATA/media_store" ]; then
    find "$DATA/media_store" -mindepth 1 -delete
fi

# Without DENDRITE_MEDIA (Dendrite's media_api.base_path), generate
# placeholder files, and point the scratch Dendrite database at them.
if [ -z "${DENDRITE_MEDIA:-}" ]; then
    DENDRITE_MEDIA=/tmp/den2syn/dev/dendrite-media
    mkdir -p "$DENDRITE_MEDIA"
    docker run --rm --user "$(id -u):$(id -g)" --network "$NETWORK" \
        -v "$PWD/scripts":/scripts:ro -v "$DENDRITE_MEDIA":/dendrite-media \
        --entrypoint python "$SYNAPSE_IMAGE" \
        /scripts/dev_media_placeholders.py "host=$PG user=matrix password=matrix dbname=dendrite" /dendrite-media
fi

exec docker run --rm --user "$(id -u):$(id -g)" --network "$NETWORK" \
    -v "$PWD":/den2syn:ro -v "$DATA":/data -v "$DENDRITE_MEDIA":/dendrite-media:ro -w /den2syn \
    -e PYTHONDONTWRITEBYTECODE=1 \
    --entrypoint python "$SYNAPSE_IMAGE" \
    -m den2syn --config /data/homeserver.yaml \
    --dendrite-db "host=$PG user=matrix password=matrix dbname=dendrite" \
    --dendrite-media /dendrite-media "$@"
