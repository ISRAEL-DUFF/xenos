-- name: SyncHost :exec
INSERT INTO hosts (name) VALUES ($1) ON CONFLICT (name) DO NOTHING;

-- name: ListHosts :many
SELECT name, status FROM hosts ORDER BY name;

-- name: SetHostStatus :execrows
UPDATE hosts SET status = $2 WHERE name = $1;

-- name: HostOfVMID :one
SELECT host FROM vms WHERE proxmox_vmid = $1 LIMIT 1;

-- name: SetHostTemplate :exec
INSERT INTO host_templates (host, template_id, proxmox_template_id) VALUES ($1, $2, $3)
ON CONFLICT (host, template_id) DO UPDATE SET proxmox_template_id = EXCLUDED.proxmox_template_id;

-- name: ListHostTemplates :many
SELECT ht.host, t.slug, t.active, ht.proxmox_template_id FROM host_templates ht JOIN templates t ON t.id = ht.template_id ORDER BY ht.host, t.id;

-- name: HostVMCounts :many
SELECT host, count(*)::int AS vms FROM vms WHERE state NOT IN ('deleted') GROUP BY host;

-- name: HostFreeIPs :many
SELECT host, count(*)::int AS free FROM ip_addresses WHERE vm_id IS NULL GROUP BY host;

-- name: HostCommittedRAM :many
-- RAM promised to live VMs per host, in MB.
SELECT v.host, COALESCE(sum(p.ram_mb), 0)::bigint AS ram_mb
FROM vms v JOIN plans p ON p.id = v.plan_id
WHERE v.state IN ('pending', 'provisioning', 'running', 'stopped') GROUP BY v.host;

-- name: HostTemplateVMIDInUse :one
SELECT EXISTS (SELECT 1 FROM host_templates WHERE host = $1 AND proxmox_template_id = $2 AND template_id <> $3);

-- name: HostHasTemplate :one
SELECT EXISTS (SELECT 1 FROM host_templates WHERE host = $1 AND template_id = $2);

-- name: SpreadGroupHosts :many
-- Hosts already holding a live VM of this account's spread group.
SELECT DISTINCT host FROM vms
WHERE user_id = $1 AND spread_group = $2 AND state NOT IN ('deleted', 'deleting', 'error');
