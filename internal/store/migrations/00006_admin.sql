-- +goose Up
-- Manual balance changes made by an admin, each backed by an iSpend movement.
CREATE TABLE adjustments (
    id                 BIGSERIAL PRIMARY KEY,
    admin_id           BIGINT NOT NULL REFERENCES users(id),
    user_id            BIGINT NOT NULL REFERENCES users(id),
    amount_uusdt       BIGINT NOT NULL CHECK (amount_uusdt <> 0), -- positive credits the customer, negative debits
    note               TEXT NOT NULL,
    ispend_movement_id TEXT,
    status             TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'complete', 'failed')),
    last_error         TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX adjustments_user_idx ON adjustments(user_id, id DESC);

-- Who did what in the admin area.
CREATE TABLE admin_audit (
    id       BIGSERIAL PRIMARY KEY,
    admin_id BIGINT NOT NULL REFERENCES users(id),
    action   TEXT NOT NULL,
    target   TEXT NOT NULL,
    detail   JSONB NOT NULL DEFAULT '{}',
    at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX admin_audit_at_idx ON admin_audit(at DESC);

-- +goose Down
DROP TABLE admin_audit, adjustments;
