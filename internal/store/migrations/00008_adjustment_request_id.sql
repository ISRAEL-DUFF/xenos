-- +goose Up
-- A client-chosen id makes an admin adjustment retryable: the same request_id from the same admin is the same
-- adjustment, so a retry after a crash or timeout cannot credit twice.
ALTER TABLE adjustments ADD COLUMN request_id TEXT;
CREATE UNIQUE INDEX adjustments_request_idx ON adjustments(admin_id, request_id) WHERE request_id IS NOT NULL;

-- +goose Down
DROP INDEX adjustments_request_idx;
ALTER TABLE adjustments DROP COLUMN request_id;
