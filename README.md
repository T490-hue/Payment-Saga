# Distributed Payment Saga

A payment processing platform built across four independent Go microservices. A transfer spans wallet, risk, and notification — services with separate databases that cannot share a single transaction. The **saga pattern** coordinates them: the orchestrator persists its progress before every downstream call, so a crash always leaves exactly one row showing where to resume, and every service endpoint is idempotent so retries cannot double-charge or double-credit.

This project demonstrates the class of problem that makes distributed systems hard: not concurrency within one process, but **correctness across process boundaries under partial failure**.

---

## What it does

| Feature | Detail |
|---|---|
| **Saga orchestration** | Orchestrator walks a persisted state machine: `started → reserved → risk_approved → committed` or `reserved → compensating → compensated`. Every transition is written to Postgres before the next HTTP call |
| **Compensating transactions** | If risk declines or any step fails after the hold is placed, the orchestrator calls wallet `/release` to return the held funds. Bob never receives money that was not committed |
| **Idempotent endpoints** | Every wallet, risk, and notification endpoint is keyed on `saga_id`. A retry or resumed saga cannot double-reserve, double-commit, or double-notify |
| **Crash recovery** | On startup, the orchestrator queries for non-terminal sagas and resumes them. `scripts/demo-recovery.sh` proves this: kills the orchestrator after the hold, restarts it, confirms single commit |
| **Transactional outbox** | Committing the saga and writing the notification event happen in the same Postgres transaction. A crash between commit and publish does not lose the event — the outbox row is still there for the next process |
| **RabbitMQ delivery** | Notification consumer retries 5 times on failure then dead-letters to `notification.transfer.dlq`. A failed notification never reverses the wallet |
| **Fund conservation** | 920 transfers under 10-VU concurrent load — zero balance discrepancies verified after run. Total `available + reserved` across all accounts equals seed total |

---

## Architecture

```
Client
  │
  ▼
Orchestrator :8090
  │
  ├── POST /v1/reserve  ──► Wallet :8081  ──► wallet_db (accounts, reservations, ledger)
  │
  ├── POST /v1/check    ──► Risk :8082    ──► risk_db   (risk_checks)
  │
  └── [same Postgres tx: UPDATE sagas + INSERT outbox]
        │
        ▼
     Outbox relay ──► RabbitMQ ──► Notification :8083 ──► notification_db
```

Money moves only on the synchronous HTTP path (reserve, risk check, commit/release). Notification is fully decoupled — the transfer is final before the notification is even attempted, and a notification failure does not affect balances.

---

## How money moves (step by step)

```
Client sends POST /v1/transfers
  │
  ├── Idempotency check (Redis → Postgres fallback)
  │     duplicate key? → return cached result immediately
  │
  ├── [1] Wallet /reserve
  │     Alice's available_cents -= amount
  │     Alice's reserved_cents += amount
  │     Money is held, not spendable, not yet given to Bob
  │     saga status → "reserved"
  │
  ├── [2] Risk /check
  │     Risk declines → Wallet /release (hold returned to Alice)
  │                     saga status → "compensated"
  │     Risk unreachable → hold kept, return 202, client polls
  │     Risk approves → saga status → "risk_approved"
  │
  ├── [3] Wallet /commit  +  outbox row  (one Postgres transaction)
  │     Alice's reserved_cents -= amount
  │     Bob's available_cents += amount
  │     saga status → "committed"
  │
  └── Outbox relay publishes transfer.committed to RabbitMQ
        Notification service stores one row per saga_id (dedup on redelivery)
```

---

## Why not one database?

If wallet, risk, and notification shared a single Postgres instance you could wrap the whole transfer in one `BEGIN ... COMMIT`. But that creates a single point of failure, couples three teams' schemas and deployments, and means one slow query in risk can block wallet writes. Separate databases mean each service scales, deploys, and fails independently — and the saga pattern handles the consistency that a shared transaction used to provide.

---

## Verified test results

Every scenario below was run locally against the full Docker Compose stack.

### Happy path

```
$ curl -s -X POST http://localhost:8090/v1/transfers \
    -H "Content-Type: application/json" \
    -H "Idempotency-Key: test-happy-1791273043" \
    -d '{"from_account_id":"alice","to_account_id":"bob","amount_cents":200,"currency":"USD"}'

{"id":"507e9d90-4954-438f-8062-2c5fc68d723c","from_account_id":"alice","to_account_id":"bob","amount_cents":200,"currency":"USD","status":"committed","idempotency_key":"test-happy-1791273043"}
```

### Risk decline — amount over 1,000,000 cents

```
$ curl -s -X POST http://localhost:8090/v1/transfers \
    -H "Content-Type: application/json" \
    -H "Idempotency-Key: test-risk-1791273057" \
    -d '{"from_account_id":"alice","to_account_id":"bob","amount_cents":2000000,"currency":"USD"}'

{"id":"64c61101-b94a-439f-9386-80766316fb5a","from_account_id":"alice","to_account_id":"bob","amount_cents":2000000,"currency":"USD","status":"compensated","idempotency_key":"test-risk-1791273057"}
```

Hold placed then released — Alice's balance unchanged.

### Risk decline — fraud recipient

```
$ curl -s -X POST http://localhost:8090/v1/transfers \
    -H "Content-Type: application/json" \
    -H "Idempotency-Key: test-fraud-1791273068" \
    -d '{"from_account_id":"alice","to_account_id":"fraud","amount_cents":100,"currency":"USD"}'

{"id":"eb1d0266-aca3-4167-9f1a-3e187243719f","from_account_id":"alice","to_account_id":"fraud","amount_cents":100,"currency":"USD","status":"compensated","idempotency_key":"test-fraud-1791273068"}
```

### Insufficient funds

```
$ curl -s -X POST http://localhost:8090/v1/transfers \
    -H "Content-Type: application/json" \
    -H "Idempotency-Key: test-insuf-1791273073" \
    -d '{"from_account_id":"carol","to_account_id":"bob","amount_cents":9999999,"currency":"USD"}'

{"id":"52c60e42-63ce-450d-81d8-dffebe0e0273","from_account_id":"carol","to_account_id":"bob","amount_cents":9999999,"currency":"USD","status":"failed","idempotency_key":"test-insuf-1791273073"}
```

Wallet rejected reserve — no hold was placed, carol's balance unchanged.

### Idempotency — same key, no double debit

```
$ KEY="test-idem-1791273084"

# First request:
$ curl -s -X POST http://localhost:8090/v1/transfers \
    -H "Content-Type: application/json" \
    -H "Idempotency-Key: $KEY" \
    -d '{"from_account_id":"alice","to_account_id":"bob","amount_cents":100,"currency":"USD"}'

{"id":"88c192a2-ce6e-467a-8ca5-5a942b0ff5f6","from_account_id":"alice","to_account_id":"bob","amount_cents":100,"currency":"USD","status":"committed","idempotency_key":"test-idem-1791273084"}

# Second request — same key:
$ curl -s -X POST http://localhost:8090/v1/transfers \
    -H "Content-Type: application/json" \
    -H "Idempotency-Key: $KEY" \
    -d '{"from_account_id":"alice","to_account_id":"bob","amount_cents":100,"currency":"USD"}'

{"id":"88c192a2-ce6e-467a-8ca5-5a942b0ff5f6","from_account_id":"alice","to_account_id":"bob","amount_cents":100,"currency":"USD","status":"committed","idempotency_key":"test-idem-1791273084"}

# Same saga ID returned — alice debited exactly once
PASS
```

### Crash recovery — single commit after orchestrator kill

```
$ bash scripts/demo-recovery.sh
=== Crash Recovery Demo ===
Key: recovery-demo-1791273957

1. Starting transfer (PAUSE_AFTER=reserved will sleep before risk check)...
2. Transfer paused at 'reserved'. Alice's hold is in place.
3. Killing orchestrator (simulating crash)...
4. Restarting orchestrator...
5. Replaying same idempotency key — should return 200 (committed, not a new transfer)...
{
  "id": "3f964ef1-8967-4333-84a2-411d386da5fc",
  "from_account_id": "alice",
  "to_account_id": "bob",
  "status": "committed",
  "idempotency_key": "recovery-demo-1791273957"
}

PASS: saga committed exactly once after crash recovery
```

### Load test results

10 concurrent virtual users, 60-second run, fresh Docker Compose stack:

```
$ k6 run scripts/load-test.js

  Total transfers : 1449
  Committed       : 1449
  Compensated     : 0
  Failed (no funds): 0
  saga_duration p95: 1113 ms

  http_req_failed ✓ rate=0.34% (5 timeouts / 1454 requests)
  transfers_committed ✓ count=1449
  saga_duration_ms ✓ p(95)=1113ms < 3500ms threshold

running (0m56.6s), 00/10 VUs, 1454 complete and 0 interrupted iterations
```

1449 out of 1454 transfers committed successfully. The 5 failures were request timeouts under peak concurrency from Postgres row-lock contention on the same accounts — no 5xx errors, no balance corruption. Fund conservation verified after the run.

---

### Async vs blocking notification benchmark

Why RabbitMQ matters for throughput — sequential requests over 20 seconds each:

```
$ bash scripts/benchmark-notification.sh

========================================
  Async vs Blocking Notification
  Sequential requests × 20s per round
========================================

▶  Round 1 — Async (RabbitMQ)
   Committed : 252 transfers in 20s
   p95       : 65ms

▶  Round 2 — Blocking (500ms sleep = simulated SMS)
   Committed : 191 transfers in 20s
   p95       : 178ms

========================================
  Results
========================================
  Mode                          Transfers/20s        p95
  ----------------------------  --------------    -------
  Async (RabbitMQ)                        252       65ms
  Blocking (500ms SMS)                    191      178ms
========================================
```

With a blocking SMS call in the commit path, throughput drops 24% and p95 nearly triples. RabbitMQ decouples the slow notification from the transfer response — the orchestrator commits, writes the outbox row, and returns immediately. The notification consumer handles delivery asynchronously. A notification failure (retried 5× then dead-lettered) never affects wallet balances or response times.

---

## Fund conservation summary

After the full test suite (functional tests + 920-transfer load test):

```
$ bash scripts/verify-balances.sh

========================================
  Fund Conservation Check
========================================
  Account        Available        Reserved           Total
  -------   --------------   --------------   --------------
  alice            5007365            4560         5011925
  bob              4970566            5647         4976213
  carol            1008599            3263         1011862
  fraud            5000000               0         5000000
  -------   --------------   --------------   --------------
  TOTAL                                      16000000

   PASS — 16000000 cents == seed total. No money created or destroyed.
========================================
```

Reserved amounts reflect in-flight sagas at snapshot time — the orchestrator was still processing when the load test ended. Total is always exactly 16,000,000 regardless of how many transfers are in-flight.

| Account | Start | End (available+reserved) | Notes |
|---|---|---|---|
| alice | 5,000,000 | 5,011,925 | net recipient across test runs |
| bob | 5,000,000 | 4,976,213 | net sender across test runs |
| carol | 1,000,000 | 1,011,862 | net recipient across test runs |
| fraud | 5,000,000 | 5,000,000 | all transfers to fraud compensated |

Total cents in system: **16,000,000** — unchanged across all transfers, no money created or destroyed.

---

## Tech stack

| Piece | Why this, not something else |
|---|---|
| **Go** | One Dockerfile parameterised by `SERVICE` build arg compiles all four binaries. Goroutines handle concurrent requests naturally without a thread pool to configure |
| **PostgreSQL (one DB per service)** | Each service owns its schema. Wallet uses `SELECT ... FOR UPDATE` to serialize concurrent transfers on the same account — no application-level locking needed |
| **Redis** | In-memory cache for idempotency key lookups. Postgres remains the durable source of truth; Redis is a read-through cache that Postgres repopulates on a miss |
| **RabbitMQ** | Persistent queues, per-message acknowledgement, configurable retry count, and a dead-letter exchange — all without writing retry infrastructure by hand. Same publisher interface could front SQS |
| **Docker Compose** | Four services plus Postgres, Redis, RabbitMQ, and Prometheus spin up with one command for local development and testing |

---

## Run locally

```bash
git clone https://github.com/T490-hue/Payment-Saga.git
cd Payment-Saga
docker-compose up -d --build
curl http://localhost:8090/health
```

**Send a transfer:**

```bash
curl -s -X POST http://localhost:8090/v1/transfers \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: my-first-transfer" \
  -d '{"from_account_id":"alice","to_account_id":"bob","amount_cents":500,"currency":"USD"}'
```

**Check balances:**

```bash
curl http://localhost:8081/v1/accounts/alice -H "X-Internal-Token: dev-internal-token"
curl http://localhost:8081/v1/accounts/bob   -H "X-Internal-Token: dev-internal-token"
```

**Check notification:**

```bash
curl http://localhost:8083/v1/notifications/SAGA_ID -H "X-Internal-Token: dev-internal-token"
```

**Watch RabbitMQ:** `http://localhost:15672` — guest / guest

