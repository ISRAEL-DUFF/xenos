-- +goose Up
-- billing_from: first hour not yet charged (hour-aligned, UTC); NULL when the VM is not billable.
-- billing_until: when billing stopped (suspension or deletion); hours up to and including its hour are still owed.
ALTER TABLE vms ADD COLUMN billing_from  TIMESTAMPTZ;
ALTER TABLE vms ADD COLUMN billing_until TIMESTAMPTZ;

-- grace_started_at: first failed charge; VMs are suspended now and deleted after the grace period.
ALTER TABLE users ADD COLUMN grace_started_at TIMESTAMPTZ;
ALTER TABLE users ADD COLUMN low_balance_notified_at TIMESTAMPTZ;

ALTER TABLE usage_charges ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT now();
CREATE INDEX usage_charges_open_idx ON usage_charges(hour) WHERE status IN ('pending', 'unpaid');
CREATE INDEX usage_charges_user_idx ON usage_charges(user_id, hour DESC);

ALTER TABLE conversions ADD COLUMN last_error TEXT;
CREATE INDEX conversions_user_idx ON conversions(user_id, created_at DESC);

-- Quotes shown to a customer, kept so a confirmed conversion uses exactly what was displayed.
CREATE TABLE conversion_quotes (
    id              TEXT PRIMARY KEY,
    user_id         BIGINT NOT NULL REFERENCES users(id),
    amount_ngn_kobo BIGINT NOT NULL,
    amount_uusdt    BIGINT NOT NULL,
    rate            TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE conversion_quotes;
DROP INDEX conversions_user_idx, usage_charges_user_idx, usage_charges_open_idx;
ALTER TABLE conversions DROP COLUMN last_error;
ALTER TABLE usage_charges DROP COLUMN created_at;
ALTER TABLE users DROP COLUMN low_balance_notified_at, DROP COLUMN grace_started_at;
ALTER TABLE vms DROP COLUMN billing_until, DROP COLUMN billing_from;
