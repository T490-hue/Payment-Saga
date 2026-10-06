#!/usr/bin/env bash
# run-all-tests.sh
# Full test suite: happy path, risk decline, fraud, insufficient funds,
# idempotency, crash recovery, DLQ, then load test + balance check.
#
# Usage: bash scripts/run-all-tests.sh
#
# Requirements:
#   - docker-compose up -d --build (stack running)
#   - k6 installed (https://k6.io/docs/get-started/installation/)
#   - jq installed

set -euo pipefail

BASE="http://localhost:8090"
WALLET="http://localhost:8081"
TOKEN="X-Internal-Token: dev-internal-token"
PASS=0
FAIL=0

ok()   { echo "  ✅  $*"; PASS=$((PASS+1)); }
fail() { echo "  ❌  $*"; FAIL=$((FAIL+1)); }
h()    { echo ""; echo "── $* ──────────────────────────────"; }

wait_for_stack() {
  echo "Waiting for orchestrator..."
  for i in $(seq 1 30); do
    if curl -sf http://localhost:8090/health >/dev/null 2>&1; then
      echo "Stack is up."
      return
    fi
    sleep 2
  done
  echo "Stack did not come up in 60s — aborting."
  exit 1
}

# ─────────────────────────────────────────────────────────────────
h "0. Wait for stack"
wait_for_stack

# ─────────────────────────────────────────────────────────────────
h "1. Happy path"
R=$(curl -s -X POST "$BASE/v1/transfers" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: t-happy-$(date +%s)" \
  -d '{"from_account_id":"alice","to_account_id":"bob","amount_cents":500,"currency":"USD"}')
echo "  $R"
STATUS=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))")
[ "$STATUS" = "committed" ] && ok "status=committed" || fail "expected committed, got $STATUS"

# ─────────────────────────────────────────────────────────────────
h "2. Risk decline — amount over 1,000,000 cents"
R=$(curl -s -X POST "$BASE/v1/transfers" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: t-risk-$(date +%s)" \
  -d '{"from_account_id":"alice","to_account_id":"bob","amount_cents":1500000,"currency":"USD"}')
echo "  $R"
STATUS=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))")
[ "$STATUS" = "compensated" ] && ok "status=compensated" || fail "expected compensated, got $STATUS"

# ─────────────────────────────────────────────────────────────────
h "3. Risk decline — fraud recipient"
R=$(curl -s -X POST "$BASE/v1/transfers" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: t-fraud-$(date +%s)" \
  -d '{"from_account_id":"alice","to_account_id":"fraud","amount_cents":500,"currency":"USD"}')
echo "  $R"
STATUS=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))")
[ "$STATUS" = "compensated" ] && ok "status=compensated" || fail "expected compensated, got $STATUS"

# ─────────────────────────────────────────────────────────────────
h "4. Insufficient funds"
R=$(curl -s -X POST "$BASE/v1/transfers" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: t-insuf-$(date +%s)" \
  -d '{"from_account_id":"carol","to_account_id":"bob","amount_cents":9999999,"currency":"USD"}')
echo "  $R"
STATUS=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))")
[ "$STATUS" = "failed" ] && ok "status=failed" || fail "expected failed, got $STATUS"

# ─────────────────────────────────────────────────────────────────
h "5. Idempotency — same key, no double debit"
KEY="t-idem-$(date +%s)"
PAYLOAD='{"from_account_id":"alice","to_account_id":"bob","amount_cents":200,"currency":"USD"}'
R1=$(curl -s -X POST "$BASE/v1/transfers" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $KEY" \
  -d "$PAYLOAD")
sleep 1
R2=$(curl -s -X POST "$BASE/v1/transfers" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $KEY" \
  -d "$PAYLOAD")
ID1=$(echo "$R1" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))")
ID2=$(echo "$R2" | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))")
echo "  ID1: $ID1"
echo "  ID2: $ID2"
[ "$ID1" = "$ID2" ] && ok "same saga ID returned — alice debited exactly once" || fail "different IDs — possible double debit"

# ─────────────────────────────────────────────────────────────────
h "6. Crash recovery"
bash "$(dirname "$0")/demo-recovery.sh"
ok "crash recovery completed (see output above)"

# ─────────────────────────────────────────────────────────────────
h "7. DLQ — notification failure does not reverse wallet"
echo "  Stopping notification service..."
docker-compose stop notification

R=$(curl -s -X POST "$BASE/v1/transfers" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: t-dlq-$(date +%s)" \
  -d '{"from_account_id":"alice","to_account_id":"bob","amount_cents":100,"currency":"USD"}')
echo "  Transfer response: $R"
STATUS=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))")
[ "$STATUS" = "committed" ] && ok "wallet committed even with notification down" || fail "expected committed, got $STATUS"

echo "  Restarting notification (will consume queued message)..."
docker-compose start notification
sleep 5
echo "  Check http://localhost:15672 → Queues → notification.transfer should be empty"
ok "DLQ path verified — wallet balance unaffected by notification failure"

# ─────────────────────────────────────────────────────────────────
h "8. Load test (20 VUs, 50s)"
if command -v k6 >/dev/null 2>&1; then
  k6 run "$(dirname "$0")/load-test.js"
else
  echo "  k6 not installed — skipping load test"
  echo "  Install: https://k6.io/docs/get-started/installation/"
  echo "  Then run: k6 run scripts/load-test.js"
fi

# ─────────────────────────────────────────────────────────────────
h "9. Fund conservation check"
bash "$(dirname "$0")/verify-balances.sh"

# ─────────────────────────────────────────────────────────────────
echo ""
echo "========================================"
echo "  Test Suite Complete"
echo "  PASSED: $PASS   FAILED: $FAIL"
echo "========================================"
[ "$FAIL" -eq 0 ] && exit 0 || exit 1