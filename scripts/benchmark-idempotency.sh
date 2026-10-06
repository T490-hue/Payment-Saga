#!/usr/bin/env bash
# benchmark-idempotency.sh
# Compares p95 latency under a retry-heavy workload when idempotency lookups
# go through Redis (cache) vs directly to Postgres.
#
# What to observe: p95 drops in redis mode because duplicate/retry requests
# hit an in-memory cache instead of a Postgres query.
set -e

BASE_URL=${BASE_URL:-http://localhost:8090}
DURATION=${DURATION:-30s}
VUS=${VUS:-20}

run_test() {
  local mode=$1
  echo ""
  echo "=== IDEMPOTENCY_CACHE=$mode ==="
  docker-compose stop orchestrator
  IDEMPOTENCY_CACHE=$mode docker-compose up -d orchestrator
  sleep 3

  # Send a mix of new transfers and deliberate retries (same key)
  docker run --rm --network saga_default \
    -v "$PWD/scripts/k6-idempotency.js:/k6-idempotency.js" grafana/k6 \
    run --duration "$DURATION" --vus "$VUS" \
    -e BASE_URL=http://orchestrator:8080 \
    /k6-idempotency.js 2>&1 | grep -E "p\(95\)|req_duration|http_reqs"
}

echo "Benchmark: Redis-cached vs Postgres-only idempotency lookups"
echo "Same workload (50% new requests, 50% retries), only IDEMPOTENCY_CACHE changes."
echo "Retry requests are the ones that benefit: cache hit = sub-ms vs Postgres round-trip."
echo ""

run_test postgres
run_test redis

echo ""
echo "Done. Redis p95 should be lower on retry-heavy traffic."
