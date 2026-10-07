-- +goose Up
-- API tokens: long-lived, named, revocable credentials for programs (for example PGDock provisioning nodes).
-- Only the SHA-256 of the token is stored. A token can manage VMs and read the wallet but cannot change the
-- account, move money or reach the admin area.
CREATE TABLE api_tokens (
    id           BIGSERIAL PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    prefix       TEXT NOT NULL,          -- first characters, to recognise a token in a list
    token_hash   BYTEA NOT NULL UNIQUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    expires_at   TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);
CREATE INDEX api_tokens_user_idx ON api_tokens(user_id);

-- Labels let a program find its own VMs. client_token makes creation at-most-once per user: repeating a create
-- with the same Idempotency-Key returns the VM the first call made. The boot script runs once as root through
-- the guest agent after the VM is up (its text is erased once it has run; only the outcome is kept).
ALTER TABLE vms ADD COLUMN labels JSONB NOT NULL DEFAULT '{}';
ALTER TABLE vms ADD COLUMN client_token TEXT;
ALTER TABLE vms ADD COLUMN boot_script TEXT;
ALTER TABLE vms ADD COLUMN boot_script_status TEXT NOT NULL DEFAULT 'none'
    CHECK (boot_script_status IN ('none', 'pending', 'running', 'ok', 'failed'));
ALTER TABLE vms ADD COLUMN boot_script_exit INT;
ALTER TABLE vms ADD COLUMN boot_script_output TEXT;
ALTER TABLE vms ADD COLUMN rebuild_boot_script TEXT;
CREATE UNIQUE INDEX vms_client_token_idx ON vms(user_id, client_token) WHERE client_token IS NOT NULL;

-- +goose Down
DROP INDEX vms_client_token_idx;
ALTER TABLE vms DROP COLUMN rebuild_boot_script, DROP COLUMN boot_script_output, DROP COLUMN boot_script_exit,
    DROP COLUMN boot_script_status, DROP COLUMN boot_script, DROP COLUMN client_token, DROP COLUMN labels;
DROP TABLE api_tokens;
