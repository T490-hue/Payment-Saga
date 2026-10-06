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
| **Idempotency cache benchmark** | `IDEMPOTENCY_CACHE=redis` (default) vs `postgres` — Redis caches processed keys so retries hit memory instead of Postgres. `scripts/benchmark-idempotency.sh` measures the p95 difference under retry-heavy load |
| **Fund conservation** | 20 parallel transfers all commit with no lost updates — verified by checking total `available + reserved` across all accounts equals the seed total |

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

## Idempotency cache benchmark

Every transfer requires an `Idempotency-Key` header. When the orchestrator sees a key it has already processed, it returns the original result without re-running the saga. The lookup goes to Redis first (`IDEMPOTENCY_CACHE=redis`, default) or directly to Postgres (`IDEMPOTENCY_CACHE=postgres`).

Under retry-heavy traffic — network blips, client timeouts, duplicate requests — the cache hit rate is high and Redis's sub-millisecond response time directly reduces client-visible p95 latency.

```bash
bash scripts/benchmark-idempotency.sh
```

The script runs the same k6 workload (50% new requests, 50% retries of the same key) against both modes and prints p95 side by side. The difference is one Postgres round-trip removed per duplicate request.

---

## Crash recovery demo

```bash
bash scripts/demo-recovery.sh
```

The script:
1. Starts a transfer with `PAUSE_AFTER=reserved` so the orchestrator sleeps after placing the hold
2. Kills the orchestrator (`docker kill`)
3. Restarts it
4. Replays the same idempotency key

Expected output: the saga commits exactly once, Alice's balance decreases by the amount exactly once, replaying the key returns `200` (idempotent replay, not a new transfer).

---

## Verified failure scenarios

| Scenario | What happens |
|---|---|
| Risk declines (`fraud`, amount > 1M cents) | Hold released, balances restored, `transfer.failed` published to RabbitMQ |
| Risk container stopped mid-transfer | Hold kept, `202` returned, saga stays `reserved`. Same key commits once risk restarts |
| Orchestrator killed after hold, before risk check | On restart, orchestrator reads `status=reserved`, resumes at risk check, commits exactly once |
| Notification fails 5 times | RabbitMQ moves message to `notification.transfer.dlq`. Wallet balance unchanged |
| 20 parallel transfers | All commit. `available + reserved` across all accounts equals 16,000,000 (seed total) |
| Duplicate idempotency key | Returns `200` with original result, no second debit |

---

## Tech stack

| Piece | Why this, not something else |
|---|---|
| **Go** | One Dockerfile parameterised by `SERVICE` build arg compiles all four binaries. Goroutines handle concurrent requests naturally without a thread pool to configure |
| **PostgreSQL (one DB per service)** | Each service owns its schema. Wallet uses `SELECT ... FOR UPDATE` to serialize concurrent transfers on the same account — no application-level locking needed |
| **Redis** | In-memory cache for idempotency key lookups. Postgres remains the durable source of truth; Redis is a read-through cache that Postgres repopulates on a miss |
| **RabbitMQ** | Persistent queues, per-message acknowledgement, configurable retry count, and a dead-letter exchange — all without writing retry infrastructure by hand. Same publisher interface could front SQS |
| **Docker Compose** | Four services plus Postgres, Redis, RabbitMQ, and Prometheus spin up with one command for local development and testing |
| **Prometheus** | Prometheus scrapes all four services, giving visibility across the saga's full path |

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

---

## Seed accounts

| Account | Starting balance (cents) | Risk behavior |
|---|---|---|
| alice | 5,000,000 | Normal |
| bob | 5,000,000 | Normal |
| carol | 1,000,000 | Normal |
| fraud | 5,000,000 | Always declined by risk |

---

## API (orchestrator)

| Method | Path | Header | Description |
|---|---|---|---|
| `GET` | `/health` | — | Liveness |
| `POST` | `/v1/transfers` | `Idempotency-Key` required | Start a transfer |
| `GET` | `/v1/transfers` | — | List recent sagas |
| `GET` | `/v1/transfers/:id` | — | Get one saga by ID |

Responses: `201` committed, `200` idempotent replay, `202` in-progress (poll), `422` declined/insufficient.

---

## Ports

| Service | Host port |
|---|---|
| Orchestrator | 8090 |
| Wallet | 8081 |
| Risk | 8082 |
| Notification | 8083 |
| RabbitMQ management | 15672 |
| Prometheus | 9091 |

---

## File layout

```
cmd/orchestrator/     saga coordinator, crash recovery on startup, outbox relay
cmd/wallet/           reserve / commit / release with SELECT FOR UPDATE, idempotent on saga_id
cmd/risk/             approve or decline with audit log in risk_db
cmd/notification/     RabbitMQ consumer, deduplicates on saga_id + event_type
internal/saga/        Transfer type and status constants shared across services
internal/store/       IdempotencyStore — Redis cache with Postgres fallback, benchmark toggle
internal/mq/          RabbitMQ publisher, consumer, DLX/DLQ wiring
scripts/schema.sql    DDL for all four databases plus seed accounts
scripts/demo-recovery.sh    crash recovery proof: kill → restart → single commit
scripts/benchmark-idempotency.sh  Redis vs Postgres idempotency lookup p95 comparison
scripts/k6-idempotency.js   k6 workload (50% retries) for the benchmark
deploy/prometheus.yml scrape config for all four services
```

---

> Credentials in docker-compose.yml are local development placeholders. See [docs/DEPLOY.md](docs/DEPLOY.md) for production hardening.
