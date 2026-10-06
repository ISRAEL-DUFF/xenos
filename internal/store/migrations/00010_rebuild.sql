-- +goose Up
-- Rebuild reinstalls a VM from a template. The target template and SSH keys are held here while the worker
-- does it, so the VM row only changes once the new guest is up.
ALTER TABLE vms DROP CONSTRAINT vms_busy_check;
ALTER TABLE vms ADD CONSTRAINT vms_busy_check CHECK (busy IN ('resizing', 'snapshotting', 'restoring', 'rebuilding'));
ALTER TABLE vms ADD COLUMN rebuild_template_id BIGINT REFERENCES templates(id);
ALTER TABLE vms ADD COLUMN rebuild_keys TEXT;

-- +goose Down
ALTER TABLE vms DROP COLUMN rebuild_keys, DROP COLUMN rebuild_template_id;
ALTER TABLE vms DROP CONSTRAINT vms_busy_check;
ALTER TABLE vms ADD CONSTRAINT vms_busy_check CHECK (busy IN ('resizing', 'snapshotting', 'restoring'));
