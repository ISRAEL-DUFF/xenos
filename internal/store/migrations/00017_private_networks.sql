-- +goose Up
-- Private networks: a /24 from 10.64.0.0/10 and a VLAN id of their own, per account. A VM joins one with a second NIC
-- (net1 or net2) on the private bridge, tagged with the VLAN. The network is pinned to the host of its first member
-- (VLANs do not cross standalone hosts without a tunnel the operator sets up) and unpinned when it empties.
CREATE TABLE private_networks (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users(id),
    name       TEXT NOT NULL,
    cidr       CIDR NOT NULL UNIQUE,
    vlan_id    INT NOT NULL UNIQUE,
    host       TEXT REFERENCES hosts(name),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, name)
);
CREATE TABLE vm_private_ips (
    id         BIGSERIAL PRIMARY KEY,
    vm_id      BIGINT NOT NULL REFERENCES vms(id),
    network_id BIGINT NOT NULL REFERENCES private_networks(id),
    slot       SMALLINT NOT NULL CHECK (slot IN (1, 2)),
    address    INET NOT NULL,
    -- attaching and detaching are being applied by the worker (the VM is busy meanwhile)
    state      TEXT NOT NULL DEFAULT 'attached' CHECK (state IN ('attached', 'attaching', 'detaching')),
    UNIQUE (network_id, address),
    UNIQUE (vm_id, slot),
    UNIQUE (vm_id, network_id)
);
CREATE INDEX vm_private_ips_network_idx ON vm_private_ips(network_id);

ALTER TABLE vms DROP CONSTRAINT vms_busy_check;
ALTER TABLE vms ADD CONSTRAINT vms_busy_check CHECK (busy IN ('resizing', 'snapshotting', 'restoring', 'rebuilding', 'networking'));

-- +goose Down
ALTER TABLE vms DROP CONSTRAINT vms_busy_check;
ALTER TABLE vms ADD CONSTRAINT vms_busy_check CHECK (busy IN ('resizing', 'snapshotting', 'restoring', 'rebuilding'));
DROP TABLE vm_private_ips;
DROP TABLE private_networks;
