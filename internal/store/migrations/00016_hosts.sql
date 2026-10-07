-- +goose Up
-- Several standalone Proxmox hosts behind one control plane. Connection details and secrets live in the hosts
-- file; this table holds what an operator changes at runtime.
CREATE TABLE hosts (
    name       TEXT PRIMARY KEY,
    status     TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'draining', 'disabled')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO hosts (name) VALUES ('default');

-- Every VM lives on one host for life (moving between hosts is not supported). VMIDs stay unique across hosts
-- (one global sequence), which is what lets a VMID identify its host.
ALTER TABLE vms ADD COLUMN host TEXT NOT NULL DEFAULT 'default' REFERENCES hosts(name);
ALTER TABLE vms ADD COLUMN spread_group TEXT;
CREATE INDEX vms_host_idx ON vms(host) WHERE state NOT IN ('deleted');
CREATE INDEX vms_spread_idx ON vms(user_id, spread_group) WHERE spread_group IS NOT NULL AND state NOT IN ('deleted', 'error');

-- Each host has its own routed subnet.
ALTER TABLE ip_addresses ADD COLUMN host TEXT NOT NULL DEFAULT 'default' REFERENCES hosts(name);
DROP INDEX ip_addresses_free_idx;
CREATE INDEX ip_addresses_free_idx ON ip_addresses(region, host, id) WHERE vm_id IS NULL;

-- A template is a VMID on a host. templates.proxmox_template_id remains the one on the 'default' host.
CREATE TABLE host_templates (
    host                TEXT NOT NULL REFERENCES hosts(name),
    template_id         BIGINT NOT NULL REFERENCES templates(id),
    proxmox_template_id INT NOT NULL,
    PRIMARY KEY (host, template_id)
);
INSERT INTO host_templates (host, template_id, proxmox_template_id) SELECT 'default', id, proxmox_template_id FROM templates;
-- The same VMID may hold a template on another host.
ALTER TABLE templates DROP CONSTRAINT templates_proxmox_template_id_key;

-- +goose Down
ALTER TABLE templates ADD CONSTRAINT templates_proxmox_template_id_key UNIQUE (proxmox_template_id);
DROP TABLE host_templates;
DROP INDEX ip_addresses_free_idx;
ALTER TABLE ip_addresses DROP COLUMN host;
CREATE INDEX ip_addresses_free_idx ON ip_addresses(region, id) WHERE vm_id IS NULL;
DROP INDEX vms_spread_idx, vms_host_idx;
ALTER TABLE vms DROP COLUMN spread_group, DROP COLUMN host;
DROP TABLE hosts;
