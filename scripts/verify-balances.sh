#!/usr/bin/env bash
# verify-balances.sh
# Run AFTER load test to confirm fund conservation:
#   total (available + reserved) across all accounts == seed total
#
# Usage: bash scripts/verify-balances.sh

set -euo pipefail

WALLET="http://localhost:8081"
TOKEN="X-Internal-Token: dev-internal-token"

ACCOUNTS=("alice" "bob" "carol" "fraud")
# Seed totals (cents) — must match schema.sql
declare -A SEED
SEED[alice]=5000000
SEED[bob]=5000000
SEED[carol]=1000000
SEED[fraud]=5000000

TOTAL_SEED=0
for acc in "${ACCOUNTS[@]}"; do
  TOTAL_SEED=$((TOTAL_SEED + SEED[$acc]))
done

echo ""
echo "========================================"
echo "  Fund Conservation Check"
echo "========================================"
printf "  %-8s  %14s  %14s  %14s\n" "Account" "Available" "Reserved" "Total"
echo "  -------   --------------   --------------   --------------"

GRAND_TOTAL=0

for acc in "${ACCOUNTS[@]}"; do
  RESP=$(curl -sf "${WALLET}/v1/accounts/${acc}" -H "${TOKEN}" 2>/dev/null || echo '{"available_cents":0,"reserved_cents":0}')
  AVAIL=$(echo "$RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('available_cents',0))")
  RESV=$(echo  "$RESP" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('reserved_cents',0))")
  TOT=$((AVAIL + RESV))
  GRAND_TOTAL=$((GRAND_TOTAL + TOT))
  printf "  %-8s  %14d  %14d  %14d\n" "$acc" "$AVAIL" "$RESV" "$TOT"
done

echo "  -------   --------------   --------------   --------------"
printf "  %-8s  %41d\n" "TOTAL" "$GRAND_TOTAL"
echo ""

if [ "$GRAND_TOTAL" -eq "$TOTAL_SEED" ]; then
  echo "  ✅  PASS — ${GRAND_TOTAL} cents == seed total. No money created or destroyed."
else
  DIFF=$((GRAND_TOTAL - TOTAL_SEED))
  echo "  ❌  FAIL — Grand total ${GRAND_TOTAL}, seed was ${TOTAL_SEED}, diff=${DIFF}"
  exit 1
fi
echo "========================================"
echo ""
