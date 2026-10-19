#!/bin/bash
# dev-client-test.sh - Runs the client test against the dev loop's Synapse.
#
# Starts Synapse on the database scripts/dev-migrate.sh migrated into, runs
# tests/client_test.py, and stops Synapse again.
#
# Usage: [DENDRITE_MEDIA=<dir>] [PASSWORD_<localpart>=...] [RECOVERY_KEY_<localpart>=...] scripts/dev-client-test.sh
# DENDRITE_MEDIA must be what dev-migrate.sh used.

set -euo pipefail

cd "$(dirname "$0")/.."

SYNAPSE_IMAGE=matrixdotorg/synapse:v1.163.0
NETWORK=den2syn-dev
PG=den2syn-dev-pg
SYNAPSE=den2syn-dev-synapse
DATA=/tmp/den2syn/dev/data
DENDRITE_MEDIA=${DENDRITE_MEDIA:-/tmp/den2syn/dev/dendrite-media}

docker run -d --rm --name "$SYNAPSE" --user "$(id -u):$(id -g)" --network "$NETWORK" \
    -v "$DATA":/data -w /data \
    --entrypoint python "$SYNAPSE_IMAGE" -m synapse.app.homeserver -c /data/homeserver.yaml >/dev/null
trap 'docker stop "$SYNAPSE" >/dev/null' EXIT
until docker exec "$SYNAPSE" curl -sf http://localhost:8008/health >/dev/null 2>&1; do sleep 1; done

# Pass through passwords and recovery keys by name only, so they don't end up
# in the command line.
env_args=()
for name in $(compgen -e | grep -E '^(PASSWORD|RECOVERY_KEY)_' || true); do
    env_args+=(-e "$name")
done

docker run --rm --user "$(id -u):$(id -g)" --network "$NETWORK" \
    -v "$PWD":/den2syn:ro -v "$DENDRITE_MEDIA":/dendrite-media:ro -w /den2syn \
    -e PYTHONDONTWRITEBYTECODE=1 "${env_args[@]}" \
    --entrypoint python "$SYNAPSE_IMAGE" \
    -m tests.client_test --synapse "http://$SYNAPSE:8008" \
    --dendrite-db "host=$PG user=matrix password=matrix dbname=dendrite" \
    --dendrite-media /dendrite-media
