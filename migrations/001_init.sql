CREATE TABLE IF NOT EXISTS users (
    id BIGSERIAL PRIMARY KEY,
    key_hash TEXT NOT NULL UNIQUE,
    label TEXT NOT NULL DEFAULT '',
    balance_micro_usdc BIGINT NOT NULL DEFAULT 0 CHECK (balance_micro_usdc >= 0),
    rate_limit_per_minute INTEGER NOT NULL CHECK (rate_limit_per_minute > 0),
    concurrency_limit INTEGER NOT NULL CHECK (concurrency_limit > 0),
    revoked BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS topups (
    id BIGSERIAL PRIMARY KEY,
    key_hash TEXT NOT NULL REFERENCES users(key_hash),
    amount_micro_usdc BIGINT NOT NULL CHECK (amount_micro_usdc > 0),
    transaction_hash TEXT NOT NULL UNIQUE,
    payer TEXT NOT NULL DEFAULT '',
    network TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS request_ledger (
    id BIGSERIAL PRIMARY KEY,
    key_hash TEXT NOT NULL REFERENCES users(key_hash),
    prompt_tokens INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    cost_micro_usdc BIGINT NOT NULL CHECK (cost_micro_usdc >= 0),
    latency_ms BIGINT NOT NULL DEFAULT 0,
    partial BOOLEAN NOT NULL DEFAULT FALSE,
    balance_after_micro_usdc BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS topups_key_hash_created_at_idx ON topups(key_hash, created_at DESC);
CREATE INDEX IF NOT EXISTS request_ledger_key_hash_created_at_idx ON request_ledger(key_hash, created_at DESC);
