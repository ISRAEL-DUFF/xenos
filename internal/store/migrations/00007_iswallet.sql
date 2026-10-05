-- +goose Up
-- The customer's iswallet virtual account, saved when it is issued. iswallet has no endpoint to
-- read it back, only to (idempotently) issue it, so we keep our own copy.
ALTER TABLE users ADD COLUMN va_bank           TEXT;
ALTER TABLE users ADD COLUMN va_account_number TEXT;
ALTER TABLE users ADD COLUMN va_account_name   TEXT;

-- Quotes live 60 seconds at iswallet; remember when, so we never offer or execute a stale one.
ALTER TABLE conversion_quotes ADD COLUMN expires_at TIMESTAMPTZ;

-- iswallet rejects a repeated idempotency key with a different payload, so each conversion remembers
-- the quote it is executing and which attempt (key) it is on. A retry replays the SAME quote and key;
-- only when that quote expired without ever executing do we take a fresh quote under a new key.
ALTER TABLE conversions ADD COLUMN convert_attempt INT NOT NULL DEFAULT 1;

-- Confirmed deposits that iswallet later clawed back (NIP recall, bank correction).
-- iswallet bears the loss and the customer keeps their credit; we record it for audit and alert.
CREATE TABLE deposit_reversals (
    id                BIGSERIAL PRIMARY KEY,
    event_key         TEXT NOT NULL UNIQUE,
    user_id           BIGINT REFERENCES users(id),
    wallet_id         TEXT NOT NULL,
    amount_kobo       BIGINT NOT NULL,
    uncovered_kobo    BIGINT NOT NULL DEFAULT 0,
    original_ref      TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE deposit_reversals;
ALTER TABLE conversions DROP COLUMN convert_attempt;
ALTER TABLE conversion_quotes DROP COLUMN expires_at;
ALTER TABLE users DROP COLUMN va_account_name, DROP COLUMN va_account_number, DROP COLUMN va_bank;
