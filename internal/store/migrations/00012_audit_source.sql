-- +goose Up
-- Operator commands (xenosctl) are audited too; they have no admin user, so admin_id may be empty and
-- source says who acted (for example "xenosctl:root").
ALTER TABLE admin_audit ALTER COLUMN admin_id DROP NOT NULL;
ALTER TABLE admin_audit ADD COLUMN source TEXT;

-- +goose Down
ALTER TABLE admin_audit DROP COLUMN source;
DELETE FROM admin_audit WHERE admin_id IS NULL;
ALTER TABLE admin_audit ALTER COLUMN admin_id SET NOT NULL;
