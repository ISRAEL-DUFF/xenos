-- +goose Up
-- Floating IPs: addresses from their own pool that an account holds (and pays for by the hour) and points at
-- one of its VMs. A row with no user_id is free in the pool. vm_id is where the account wants it;
-- applied_vm_id is where the worker last configured it, so a move knows what to take off.
CREATE TABLE floating_ips (
    id            BIGSERIAL PRIMARY KEY,
    address       INET NOT NULL UNIQUE,
    region        TEXT NOT NULL,
    user_id       BIGINT REFERENCES users(id),
    vm_id         BIGINT REFERENCES vms(id),
    applied_vm_id BIGINT REFERENCES vms(id),
    applied_at    TIMESTAMPTZ,
    label         TEXT NOT NULL DEFAULT '',
    allocated_at  TIMESTAMPTZ,
    billing_user_id BIGINT REFERENCES users(id), -- who is charged until billing finishes (user_id is NULL once released)
    billing_from  TIMESTAMPTZ,
    billing_until TIMESTAMPTZ,
    CHECK (vm_id IS NULL OR user_id IS NOT NULL)
);
CREATE INDEX floating_ips_user_idx ON floating_ips(user_id) WHERE user_id IS NOT NULL;
CREATE INDEX floating_ips_vm_idx ON floating_ips(vm_id) WHERE vm_id IS NOT NULL;
CREATE INDEX floating_ips_free_idx ON floating_ips(region, id) WHERE user_id IS NULL AND billing_from IS NULL AND applied_vm_id IS NULL;

-- Charges are for a VM-hour or a floating-IP-hour.
ALTER TABLE usage_charges ALTER COLUMN vm_id DROP NOT NULL;
ALTER TABLE usage_charges ADD COLUMN floating_ip_id BIGINT REFERENCES floating_ips(id);
ALTER TABLE usage_charges ADD CONSTRAINT usage_charges_one_subject CHECK ((vm_id IS NULL) <> (floating_ip_id IS NULL));
CREATE UNIQUE INDEX usage_charges_floating_hour_uidx ON usage_charges(floating_ip_id, hour) WHERE floating_ip_id IS NOT NULL;

-- +goose Down
DROP INDEX usage_charges_floating_hour_uidx;
ALTER TABLE usage_charges DROP CONSTRAINT usage_charges_one_subject;
DELETE FROM usage_charges WHERE vm_id IS NULL;
ALTER TABLE usage_charges DROP COLUMN floating_ip_id;
ALTER TABLE usage_charges ALTER COLUMN vm_id SET NOT NULL;
DROP TABLE floating_ips;
