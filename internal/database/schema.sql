CREATE TABLE IF NOT EXISTS settings (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    price_per_kwh_micros BIGINT,
    billing_interval_hours INTEGER NOT NULL DEFAULT 24 CHECK (billing_interval_hours BETWEEN 1 AND 8760),
    next_billing_at TIMESTAMPTZ
);

INSERT INTO settings (singleton) VALUES (TRUE) ON CONFLICT (singleton) DO NOTHING;

CREATE TABLE IF NOT EXISTS users (
    id BIGSERIAL PRIMARY KEY,
    name TEXT NOT NULL,
    email TEXT NOT NULL,
    balance_cents BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    is_active BOOLEAN NOT NULL DEFAULT TRUE
);

CREATE UNIQUE INDEX IF NOT EXISTS users_email_unique ON users (LOWER(email));

CREATE TABLE IF NOT EXISTS membership_periods (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id),
    started_at TIMESTAMPTZ NOT NULL,
    ended_at TIMESTAMPTZ,
    CHECK (ended_at IS NULL OR ended_at >= started_at)
);

CREATE UNIQUE INDEX IF NOT EXISTS membership_one_open_period
    ON membership_periods (user_id) WHERE ended_at IS NULL;
CREATE INDEX IF NOT EXISTS membership_periods_time
    ON membership_periods (started_at, ended_at);

CREATE TABLE IF NOT EXISTS billing_runs (
    id BIGSERIAL PRIMARY KEY,
    period_start TIMESTAMPTZ NOT NULL,
    period_end TIMESTAMPTZ NOT NULL,
    energy_wh DOUBLE PRECISION NOT NULL DEFAULT 0,
    total_cost_cents BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS device_state (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    energy_total_wh DOUBLE PRECISION NOT NULL,
    power_w DOUBLE PRECISION NOT NULL,
    sampled_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE IF NOT EXISTS energy_intervals (
    id BIGSERIAL PRIMARY KEY,
    started_at TIMESTAMPTZ NOT NULL,
    ended_at TIMESTAMPTZ NOT NULL,
    energy_wh DOUBLE PRECISION NOT NULL CHECK (energy_wh >= 0),
    power_w DOUBLE PRECISION NOT NULL,
    price_per_kwh_micros BIGINT,
    billed_run_id BIGINT REFERENCES billing_runs(id),
    CHECK (ended_at >= started_at)
);

CREATE INDEX IF NOT EXISTS energy_intervals_unbilled
    ON energy_intervals (ended_at) WHERE billed_run_id IS NULL;
CREATE INDEX IF NOT EXISTS energy_intervals_time ON energy_intervals (ended_at);

CREATE TABLE IF NOT EXISTS billing_entries (
    id BIGSERIAL PRIMARY KEY,
    billing_run_id BIGINT NOT NULL REFERENCES billing_runs(id),
    user_id BIGINT NOT NULL REFERENCES users(id),
    energy_wh DOUBLE PRECISION NOT NULL,
    amount_cents BIGINT NOT NULL,
    balance_before_cents BIGINT NOT NULL,
    balance_after_cents BIGINT NOT NULL,
    UNIQUE (billing_run_id, user_id)
);

CREATE TABLE IF NOT EXISTS wallet_transactions (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id),
    kind TEXT NOT NULL CHECK (kind IN ('topup', 'charge')),
    amount_cents BIGINT NOT NULL,
    balance_after_cents BIGINT NOT NULL,
    billing_entry_id BIGINT REFERENCES billing_entries(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS wallet_transactions_time ON wallet_transactions (created_at);
CREATE INDEX IF NOT EXISTS wallet_transactions_user ON wallet_transactions (user_id, created_at);

CREATE TABLE IF NOT EXISTS audit_logs (
    id BIGSERIAL PRIMARY KEY,
    event TEXT NOT NULL,
    details TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS audit_logs_time ON audit_logs (created_at DESC);

CREATE TABLE IF NOT EXISTS notification_queue (
    id BIGSERIAL PRIMARY KEY,
    recipient TEXT NOT NULL,
    subject TEXT NOT NULL,
    body TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_at TIMESTAMPTZ,
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS notification_queue_pending
    ON notification_queue (next_attempt_at) WHERE sent_at IS NULL;
