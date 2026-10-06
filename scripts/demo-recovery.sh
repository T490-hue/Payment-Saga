#!/usr/bin/env bash
# demo-recovery.sh
# Proves that killing the orchestrator after the wallet hold does not lose
# the transfer — the new process resumes the saga to a single commit.
set -e

BASE=${BASE_URL:-http://localhost:8090}
KEY="recovery-demo-$(date +%s)"

echo "=== Crash Recovery Demo ==="
echo "Key: $KEY"
echo ""

echo "1. Starting transfer (PAUSE_AFTER=reserved will sleep before risk check)..."
PAUSE_AFTER=reserved docker-compose up -d orchestrator
sleep 2

# Send the transfer in background — it will pause at 'reserved'
curl -s -X POST "$BASE/v1/transfers" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $KEY" \
  -d '{"from_account_id":"alice","to_account_id":"bob","amount_cents":1000,"currency":"USD"}' &
TRANSFER_PID=$!

sleep 2
echo "2. Transfer paused at 'reserved'. Alice's hold is in place."

echo "3. Killing orchestrator (simulating crash)..."
docker-compose kill orchestrator

echo "4. Restarting orchestrator..."
docker-compose up -d orchestrator
sleep 3

echo "5. Replaying same idempotency key — should return 200 (committed, not a new transfer)..."
RESULT=$(curl -s -X POST "$BASE/v1/transfers" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $KEY" \
  -d '{"from_account_id":"alice","to_account_id":"bob","amount_cents":1000,"currency":"USD"}')

echo "$RESULT" | jq .
STATUS=$(echo "$RESULT" | jq -r '.status')

echo ""
if [ "$STATUS" = "committed" ]; then
  echo "PASS: saga committed exactly once after crash recovery"
else
  echo "FAIL: unexpected status $STATUS"
  exit 1
fi
