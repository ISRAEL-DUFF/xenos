-- +goose Up
-- busy is a one-at-a-time claim on a VM for a long operation the worker performs (resize, snapshot work,
-- restore). The API sets it atomically when it queues the job; the worker clears it when the job ends,
-- including after its last failed attempt. resize_plan_id is the plan a resize is moving to.
ALTER TABLE vms ADD COLUMN busy TEXT CHECK (busy IN ('resizing', 'snapshotting', 'restoring'));
ALTER TABLE vms ADD COLUMN resize_plan_id BIGINT REFERENCES plans(id);

-- Customer snapshots of a VM's disk (Proxmox snapshots, no RAM state). pve_name is the name on the host.
CREATE TABLE snapshots (
    id         BIGSERIAL PRIMARY KEY,
    vm_id      BIGINT NOT NULL REFERENCES vms(id),
    name       TEXT NOT NULL,
    pve_name   TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'creating' CHECK (status IN ('creating', 'ready', 'deleting', 'error')),
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX snapshots_vm_idx ON snapshots(vm_id);

-- +goose Down
DROP TABLE snapshots;
ALTER TABLE vms DROP COLUMN resize_plan_id, DROP COLUMN busy;
