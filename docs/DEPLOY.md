# Production Deployment — Distributed Payment Saga

The saga platform has more moving parts than the gateway (4 services + Postgres + Redis + RabbitMQ),
so the deployment options are slightly different.

---

## Option A: Railway (recommended for this project)

Railway supports multi-service deploys from one repo and has built-in Postgres and Redis plugins.
RabbitMQ needs a separate setup (see below).

### Steps

1. Push your repo to GitHub
2. railway.app → New Project → Deploy from GitHub repo
3. In Railway dashboard, add services for each binary using the same repo:
   - Set `SERVICE=orchestrator` as a build arg for the orchestrator service
   - Set `SERVICE=wallet` for wallet, etc.
4. Add a **Postgres** plugin (Railway managed)
5. Add a **Redis** plugin (Railway managed)
6. For RabbitMQ: use **CloudAMQP** free tier (cloudamqp.com) — get the AMQP URL
7. Set environment variables per service:

**Orchestrator:**
```
DATABASE_URL     = (Railway Postgres URL)
REDIS_ADDR       = (Railway Redis URL)
RABBITMQ_URL     = (CloudAMQP URL)
WALLET_URL       = http://wallet.railway.internal:8080
RISK_URL         = http://risk.railway.internal:8080
INTERNAL_TOKEN   = (generate: openssl rand -hex 16)
IDEMPOTENCY_CACHE = redis
GIN_MODE         = release
```

**Wallet / Risk / Notification:** similar but only the services they need.

8. Set the start command for each Railway service to the binary name:
   orchestrator, wallet, risk, notification

---

## Option B: Render

Render has a free tier for web services but with cold starts (30-50s idle).
Good enough for a portfolio demo link.

1. Create 4 web services from your GitHub repo, one per binary
2. Set the Docker build arg `SERVICE=<name>` for each
3. Add Postgres (Render managed) and Redis (Render managed)
4. Use CloudAMQP for RabbitMQ (same as Railway option)
5. Set environment variables the same as above

---

## Option C: Fly.io (multi-service, more manual)

Fly is the most production-like but requires a `fly.toml` per service and more CLI steps.
Good if you want to learn Fly; overkill for a portfolio demo.

---

## Option D: Local + ngrok (simplest, laptop must be on)

```bash
docker-compose up -d
ngrok http 8090
```

The ngrok URL tunnels to the orchestrator. Only the orchestrator is public-facing — wallet, risk,
and notification communicate over the internal Docker network, same as a real deployment.

---

## Production hardening checklist

- [ ] `INTERNAL_TOKEN` — generate a real token and require it on wallet, risk, notification
- [ ] Postgres passwords — not the Compose defaults; use generated passwords
- [ ] Redis password — set in the Redis URL
- [ ] RabbitMQ credentials — not `guest`/`guest`; create a dedicated user in CloudAMQP
- [ ] `GIN_MODE=release` — disables debug output on all services
- [ ] `/metrics` endpoint — internal only; not exposed publicly
- [ ] `IDEMPOTENCY_CACHE=redis` — keep default; Postgres fallback is automatic
- [ ] Postgres `max_connections` — tune per instance size; pool is set to 25 per service
- [ ] Dockerfile `USER app` — already set (non-root)
- [ ] HTTPS — Railway and Render terminate TLS automatically
- [ ] RabbitMQ `maxReceiveCount` — already 5 in the queue declaration; adjust per use case

---

## Verifying the live deployment

After deploy, run these against your public URL:

```bash
BASE=https://your-orchestrator-url

# Health
curl $BASE/health

# Transfer
curl -s -X POST $BASE/v1/transfers \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: live-test-1" \
  -d '{"from_account_id":"alice","to_account_id":"bob","amount_cents":100,"currency":"USD"}'

# Idempotent replay (same key, same body → 200)
curl -s -X POST $BASE/v1/transfers \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: live-test-1" \
  -d '{"from_account_id":"alice","to_account_id":"bob","amount_cents":100,"currency":"USD"}'
```
