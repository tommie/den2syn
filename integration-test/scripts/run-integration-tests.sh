#!/bin/bash
# run-integration-tests.sh - Checks the migrated Synapse as clients see it.

set -e
set -o pipefail

RESULTS_DIR="/test-results"
mkdir -p "$RESULTS_DIR"

python -m tests.client_test \
    --synapse http://synapse:8008 \
    --dendrite-db "host=postgres user=matrix password=matrix_password dbname=dendrite" \
    --dendrite-media /dendrite-media \
    2>&1 | tee "$RESULTS_DIR/client-test.log"
