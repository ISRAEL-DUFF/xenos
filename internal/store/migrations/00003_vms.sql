-- +goose Up
-- Proxmox VMIDs: below 100 is reserved by Proxmox, 9000+ is used for templates.
CREATE SEQUENCE vmid_seq START 100 MAXVALUE 8999;

-- Snapshot of the SSH keys chosen at creation; the worker injects it via cloud-init.
ALTER TABLE vms ADD COLUMN authorized_keys TEXT NOT NULL DEFAULT '';
-- Login user baked into the cloud-init config for each image.
ALTER TABLE templates ADD COLUMN ci_user TEXT NOT NULL DEFAULT 'root';

CREATE INDEX ip_addresses_free_idx ON ip_addresses(region, id) WHERE vm_id IS NULL;

-- +goose Down
DROP INDEX ip_addresses_free_idx;
ALTER TABLE templates DROP COLUMN ci_user;
ALTER TABLE vms DROP COLUMN authorized_keys;
DROP SEQUENCE vmid_seq;
