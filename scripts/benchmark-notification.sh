#!/usr/bin/env bash
# benchmark-notification.sh
# Compares async (RabbitMQ) vs blocking (500ms SMS) notification throughput
#
# Usage: bash scripts/benchmark-notification.sh

set -euo pipefail

BASE="http://localhost:8090"
DURATION=20   # seconds per round

wait_ready() {
  for i in $(seq 1 15); do
    if curl -sf "$BASE/health" >/dev/null 2>&1; then return; fi
    sleep 1
  done
  echo "orchestrator not ready" && exit 1
}

run_round() {
  local label=$1
  local tmpfile="/tmp/bench-${label}-$$.txt"
  rm -f "$tmpfile"

  local end_time=$(( $(date +%s) + DURATION ))
  local i=0

  while [ "$(date +%s)" -lt "$end_time" ]; do
    i=$((i+1))
    KEY="${label}-${i}-$$"
    START=$(date +%s%3N)
    R=$(curl -s -X POST "$BASE/v1/transfers" \
      -H "Content-Type: application/json" \
      -H "Idempotency-Key: $KEY" \
      -d '{"from_account_id":"alice","to_account_id":"bob","amount_cents":1,"currency":"USD"}' \
      --max-time 5 2>/dev/null || echo '{}')
    END=$(date +%s%3N)
    ELAPSED=$(( END - START ))
    STATUS=$(echo "$R" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status','err'))" 2>/dev/null || echo "err")
    if [ "$STATUS" = "committed" ]; then
      echo "$ELAPSED" >> "$tmpfile"
    fi
  done

  local count=0
  local p95=0
  if [ -f "$tmpfile" ]; then
    count=$(wc -l < "$tmpfile" | tr -d ' ')
    if [ "$count" -gt 0 ]; then
      p95=$(sort -n "$tmpfile" | awk -v n="$count" 'NR==int(n*0.95)+1{print; exit} END{if(NR<int(n*0.95)+1) print}')
    fi
    rm -f "$tmpfile"
  fi

  echo "${count}|${p95}"
}

echo ""
echo "========================================"
echo "  Async vs Blocking Notification"
echo "  Sequential requests × ${DURATION}s per round"
echo "========================================"

# ── Round 1: async (normal) ───────────────────────────────────────
echo ""
echo "▶  Round 1 — Async (RabbitMQ)"
echo "   Running ${DURATION}s..."
R1=$(run_round "async")
C1=$(echo "$R1" | cut -d'|' -f1)
P1=$(echo "$R1" | cut -d'|' -f2)
echo "   Committed : $C1 transfers in ${DURATION}s"
echo "   p95       : ${P1}ms"

# ── Round 2: blocking ─────────────────────────────────────────────
echo ""
echo "▶  Round 2 — Blocking (500ms sleep = simulated SMS)"

# Pass env var via docker-compose override
echo "   Restarting orchestrator with SLOW_NOTIFICATION=true..."
docker-compose stop orchestrator > /dev/null 2>&1
SLOW_NOTIFICATION=true docker-compose up -d orchestrator > /dev/null 2>&1
wait_ready
echo "   Orchestrator ready."
echo "   Running ${DURATION}s..."
R2=$(run_round "blocking")
C2=$(echo "$R2" | cut -d'|' -f1)
P2=$(echo "$R2" | cut -d'|' -f2)
echo "   Committed : $C2 transfers in ${DURATION}s"
echo "   p95       : ${P2}ms"

# ── restore ───────────────────────────────────────────────────────
echo ""
echo "Restoring normal orchestrator..."
docker-compose stop orchestrator > /dev/null 2>&1
docker-compose up -d orchestrator > /dev/null 2>&1
wait_ready

# ── results ──────────────────────────────────────────────────────
echo ""
echo "========================================"
echo "  Results"
echo "========================================"
printf "  %-28s %14s %10s\n" "Mode" "Transfers/${DURATION}s" "p95"
printf "  %-28s %14s %10s\n" "----------------------------" "--------------" "-------"
printf "  %-28s %14s %10s\n" "Async (RabbitMQ)"     "$C1" "${P1}ms"
printf "  %-28s %14s %10s\n" "Blocking (500ms SMS)"  "$C2" "${P2}ms"
echo "========================================"