-- +goose Up
-- Closing an account: it goes to 'closing' at once (no login, no new charges), personal data is removed after a
-- 30-day grace (purge_after), and the row stays, anonymised, as the anchor for the financial records that have to
-- be kept for the retention period.
ALTER TABLE users DROP CONSTRAINT users_status_check;
ALTER TABLE users ADD CONSTRAINT users_status_check CHECK (status IN ('active', 'suspended', 'banned', 'closing', 'closed'));
ALTER TABLE users ADD COLUMN closing_at  TIMESTAMPTZ;
ALTER TABLE users ADD COLUMN purge_after TIMESTAMPTZ;
ALTER TABLE users ADD COLUMN closed_at   TIMESTAMPTZ;
CREATE INDEX users_closing_idx ON users(purge_after) WHERE status = 'closing';

-- +goose Down
DROP INDEX users_closing_idx;
ALTER TABLE users DROP COLUMN closed_at, DROP COLUMN purge_after, DROP COLUMN closing_at;
UPDATE users SET status = 'banned' WHERE status IN ('closing', 'closed');
ALTER TABLE users DROP CONSTRAINT users_status_check;
ALTER TABLE users ADD CONSTRAINT users_status_check CHECK (status IN ('active', 'suspended', 'banned'));
