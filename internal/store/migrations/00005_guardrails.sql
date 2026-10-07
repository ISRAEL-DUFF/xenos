-- +goose Up
-- The customer accepted the acceptable-use policy at signup.
ALTER TABLE users ADD COLUMN aup_accepted_at TIMESTAMPTZ;

-- CPU abuse watch: cpu_high_since is when sustained high CPU began; flagged_at is set
-- once it has lasted long enough to need a human to look (usually crypto mining).
ALTER TABLE vms ADD COLUMN cpu_high_since TIMESTAMPTZ;
ALTER TABLE vms ADD COLUMN flagged_at     TIMESTAMPTZ;
ALTER TABLE vms ADD COLUMN flag_reason    TEXT;

-- Failed jobs are alerted once.
ALTER TABLE jobs ADD COLUMN alerted_at TIMESTAMPTZ;
CREATE INDEX jobs_failed_idx ON jobs(id) WHERE status = 'failed' AND alerted_at IS NULL;

-- Per-key cooldown so one problem produces one alert, not one per minute.
CREATE TABLE alerts_sent (
    key          TEXT PRIMARY KEY,
    last_sent_at TIMESTAMPTZ NOT NULL
);

-- Rejected webhook deliveries, for alerting on a wrong secret or probing.
CREATE TABLE webhook_failures (
    id BIGSERIAL PRIMARY KEY,
    at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ip TEXT NOT NULL
);
CREATE INDEX webhook_failures_at_idx ON webhook_failures(at);

-- Liveness of background processes; /readyz reports unhealthy if the worker stops ticking.
CREATE TABLE heartbeats (
    name TEXT PRIMARY KEY,
    at   TIMESTAMPTZ NOT NULL
);

-- +goose Down
DROP TABLE heartbeats, webhook_failures, alerts_sent;
DROP INDEX jobs_failed_idx;
ALTER TABLE jobs DROP COLUMN alerted_at;
ALTER TABLE vms DROP COLUMN flag_reason, DROP COLUMN flagged_at, DROP COLUMN cpu_high_since;
ALTER TABLE users DROP COLUMN aup_accepted_at;
