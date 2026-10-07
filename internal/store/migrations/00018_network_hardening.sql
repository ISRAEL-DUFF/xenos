-- +goose Up
-- A membership whose NIC could not be confirmed gone stays reserved ("stuck") so its VLAN and address cannot be
-- handed to anyone else.
ALTER TABLE vm_private_ips DROP CONSTRAINT vm_private_ips_state_check;
ALTER TABLE vm_private_ips ADD CONSTRAINT vm_private_ips_state_check CHECK (state IN ('attached', 'attaching', 'detaching', 'stuck'));

-- A deleted network's /24 and VLAN id are not reused for a day, so a stale NIC or snapshot cannot land on a new
-- tenant's segment.
CREATE TABLE network_quarantine (
    cidr   CIDR NOT NULL,
    vlan_id INT NOT NULL,
    until  TIMESTAMPTZ NOT NULL
);

-- +goose Down
DROP TABLE network_quarantine;
ALTER TABLE vm_private_ips DROP CONSTRAINT vm_private_ips_state_check;
ALTER TABLE vm_private_ips ADD CONSTRAINT vm_private_ips_state_check CHECK (state IN ('attached', 'attaching', 'detaching'));
