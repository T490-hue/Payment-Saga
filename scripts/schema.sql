-- orchestrator DB
CREATE TABLE IF NOT EXISTS sagas (
    id               UUID PRIMARY KEY,
    from_account_id  TEXT NOT NULL,
    to_account_id    TEXT NOT NULL,
    amount_cents     BIGINT NOT NULL,
    currency         TEXT NOT NULL,
    status           TEXT NOT NULL,
    idempotency_key  TEXT UNIQUE NOT NULL,
    created_at       TIMESTAMPTZ DEFAULT now(),
    updated_at       TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE IF NOT EXISTS idempotency_keys (
    key        TEXT PRIMARY KEY,
    result     TEXT NOT NULL,
    created_at TIMESTAMPTZ DEFAULT now()
);

-- outbox for guaranteed notification delivery
CREATE TABLE IF NOT EXISTS outbox (
    id         BIGSERIAL PRIMARY KEY,
    event_type TEXT NOT NULL,
    payload    TEXT NOT NULL,
    published  BOOLEAN DEFAULT false,
    created_at TIMESTAMPTZ DEFAULT now()
);

-- wallet DB
CREATE TABLE IF NOT EXISTS accounts (
    id              TEXT PRIMARY KEY,
    available_cents BIGINT NOT NULL DEFAULT 0,
    reserved_cents  BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS reservations (
    saga_id    TEXT PRIMARY KEY,
    account_id TEXT NOT NULL,
    amount     BIGINT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'pending', -- pending | committed | released
    created_at TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE IF NOT EXISTS ledger_entries (
    id         BIGSERIAL PRIMARY KEY,
    saga_id    TEXT NOT NULL,
    account_id TEXT NOT NULL,
    amount     BIGINT NOT NULL,
    entry_type TEXT NOT NULL, -- hold | commit_debit | commit_credit
    created_at TIMESTAMPTZ DEFAULT now()
);

-- seed accounts
INSERT INTO accounts (id, available_cents) VALUES
    ('alice', 5000000),
    ('bob',   5000000),
    ('carol',  1000000),
    ('fraud',  5000000)
ON CONFLICT DO NOTHING;

-- risk DB
CREATE TABLE IF NOT EXISTS risk_checks (
    id          BIGSERIAL PRIMARY KEY,
    saga_id     TEXT NOT NULL,
    amount      BIGINT,
    from_account TEXT,
    approved    BOOLEAN,
    created_at  TIMESTAMPTZ DEFAULT now()
);

-- notification DB
CREATE TABLE IF NOT EXISTS notifications (
    id         BIGSERIAL PRIMARY KEY,
    saga_id    TEXT NOT NULL,
    event_type TEXT NOT NULL,
    payload    TEXT,
    created_at TIMESTAMPTZ DEFAULT now(),
    UNIQUE (saga_id, event_type)   -- deduplicate RabbitMQ redeliveries
);
