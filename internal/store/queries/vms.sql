-- name: LockUser :one
SELECT id FROM users WHERE id = $1 FOR UPDATE;

-- name: CountActiveVMs :one
SELECT count(*) FROM vms WHERE user_id = $1 AND state NOT IN ('deleted', 'error');

-- name: SumActiveHourly :one
SELECT COALESCE(sum(p.price_uusdt_hourly), 0)::bigint
FROM vms v JOIN plans p ON p.id = v.plan_id
WHERE v.user_id = $1 AND v.state NOT IN ('deleted', 'error');

-- name: GetActivePlanBySlug :one
SELECT * FROM plans WHERE slug = $1 AND active;

-- name: GetActiveTemplateBySlug :one
SELECT * FROM templates WHERE slug = $1 AND active;

-- name: GetSSHKeysByIDs :many
SELECT public_key FROM ssh_keys WHERE user_id = $1 AND id = ANY($2::bigint[]) ORDER BY id;

-- name: CreateVM :one
INSERT INTO vms (user_id, region, plan_id, template_id, proxmox_vmid, hostname, authorized_keys,
                 labels, client_token, boot_script, boot_script_status, host, spread_group)
VALUES (sqlc.arg(user_id), sqlc.arg(region), sqlc.arg(plan_id), sqlc.arg(template_id), nextval('vmid_seq')::int,
        sqlc.arg(hostname), sqlc.arg(authorized_keys), COALESCE(sqlc.narg(labels)::jsonb, '{}'::jsonb),
        sqlc.narg(client_token), sqlc.narg(boot_script), COALESCE(NULLIF(sqlc.arg(boot_script_status)::text, ''), 'none'),
        COALESCE(NULLIF(sqlc.arg(host)::text, ''), 'default'), sqlc.narg(spread_group))
RETURNING id;

-- name: GetVMByClientToken :one
SELECT v.id, v.hostname, p.slug AS plan_slug, t.slug AS template_slug
FROM vms v JOIN plans p ON p.id = v.plan_id JOIN templates t ON t.id = v.template_id
WHERE v.user_id = $1 AND v.client_token = $2;

-- name: SetVMLabels :execrows
UPDATE vms SET labels = $3 WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL;

-- name: ClaimFreeIP :one
SELECT id, host(address)::text AS address, host(gateway)::text AS gateway
FROM ip_addresses
WHERE vm_id IS NULL AND region = sqlc.arg(region) AND host = sqlc.arg(host)
ORDER BY id
FOR UPDATE SKIP LOCKED
LIMIT 1;

-- name: AssignIP :exec
UPDATE ip_addresses SET vm_id = $2 WHERE id = $1;

-- name: SetVMIPv4 :exec
UPDATE vms SET ipv4_id = $2 WHERE id = $1;

-- name: SetVMIPv6 :exec
UPDATE vms SET ipv6 = $2 WHERE id = $1;

-- name: ReleaseIPForVM :exec
UPDATE ip_addresses SET vm_id = NULL WHERE vm_id = $1;

-- name: ListUserVMs :many
SELECT v.id, v.region, v.host, v.spread_group, v.hostname, v.state, v.ipv6, v.created_at, v.busy, v.resize_plan_id, v.labels, v.boot_script_status, v.boot_script_exit, v.boot_script_output,
       p.slug AS plan_slug, p.price_uusdt_hourly, p.id AS plan_id, p.vcpu, p.ram_mb, p.disk_gb,
       t.slug AS template_slug, t.ci_user,
       COALESCE(host(ip.address), '')::text AS ipv4
FROM vms v
JOIN plans p ON p.id = v.plan_id
JOIN templates t ON t.id = v.template_id
LEFT JOIN ip_addresses ip ON ip.id = v.ipv4_id
WHERE v.user_id = $1 AND v.deleted_at IS NULL
ORDER BY v.id DESC;

-- name: GetUserVM :one
SELECT v.id, v.region, v.host, v.spread_group, v.hostname, v.state, v.ipv6, v.created_at, v.busy, v.resize_plan_id, v.labels, v.boot_script_status, v.boot_script_exit, v.boot_script_output,
       p.slug AS plan_slug, p.price_uusdt_hourly, p.id AS plan_id, p.vcpu, p.ram_mb, p.disk_gb,
       t.slug AS template_slug, t.ci_user,
       COALESCE(host(ip.address), '')::text AS ipv4
FROM vms v
JOIN plans p ON p.id = v.plan_id
JOIN templates t ON t.id = v.template_id
LEFT JOIN ip_addresses ip ON ip.id = v.ipv4_id
WHERE v.id = $1 AND v.user_id = $2 AND v.deleted_at IS NULL;

-- name: GetVMForWork :one
SELECT v.id, v.user_id, v.host, v.hostname, v.state, v.proxmox_vmid, v.authorized_keys, v.ipv6, v.ipv4_id, v.boot_script_status,
       p.vcpu, p.ram_mb, p.disk_gb,
       COALESCE(ht.proxmox_template_id, t.proxmox_template_id)::int AS proxmox_template_id, t.ci_user,
       COALESCE(host(ip.address), '')::text AS ipv4,
       COALESCE(host(ip.gateway), '')::text AS gateway
FROM vms v
JOIN plans p ON p.id = v.plan_id
JOIN templates t ON t.id = v.template_id
LEFT JOIN host_templates ht ON ht.template_id = t.id AND ht.host = v.host
LEFT JOIN ip_addresses ip ON ip.id = v.ipv4_id
WHERE v.id = $1;

-- name: TransitionVM :execrows
UPDATE vms SET state = $3 WHERE id = $1 AND state = ANY($2::text[]);

-- name: MarkVMDeleted :exec
UPDATE vms SET state = 'deleted', deleted_at = now() WHERE id = $1;

-- name: ClaimVMBusy :execrows
-- The one-at-a-time claim: succeeds only for the owner's idle running or stopped VM.
UPDATE vms SET busy = $3, resize_plan_id = $4
WHERE id = $1 AND user_id = $2 AND busy IS NULL AND state IN ('running', 'stopped') AND deleted_at IS NULL;

-- name: ReleaseVMBusy :exec
UPDATE vms SET busy = NULL, resize_plan_id = NULL, rebuild_template_id = NULL, rebuild_keys = NULL, rebuild_boot_script = NULL WHERE id = $1;

-- name: FinishResize :execrows
UPDATE vms SET plan_id = resize_plan_id, resize_plan_id = NULL, busy = NULL
WHERE id = $1 AND busy = 'resizing' AND resize_plan_id IS NOT NULL;

-- name: GetResizeWork :one
SELECT v.id, v.host, v.state, v.busy, v.proxmox_vmid, v.resize_plan_id,
       np.vcpu AS new_vcpu, np.ram_mb AS new_ram_mb, np.disk_gb AS new_disk_gb
FROM vms v LEFT JOIN plans np ON np.id = v.resize_plan_id
WHERE v.id = $1;

-- name: CreateSnapshot :one
INSERT INTO snapshots (vm_id, name, pve_name) VALUES ($1, $2, 'pending') RETURNING id;

-- name: SetSnapshotPVEName :exec
UPDATE snapshots SET pve_name = $2 WHERE id = $1;

-- name: ListVMSnapshots :many
SELECT id, vm_id, name, pve_name, status, COALESCE(last_error, '')::text AS last_error, created_at
FROM snapshots WHERE vm_id = $1 AND status <> 'error' ORDER BY id DESC;

-- name: CountVMSnapshots :one
SELECT count(*) FROM snapshots WHERE vm_id = $1 AND status IN ('creating', 'ready', 'deleting');

-- name: GetVMSnapshot :one
SELECT id, vm_id, name, pve_name, status FROM snapshots WHERE id = $1 AND vm_id = $2;

-- name: SetSnapshotStatus :exec
UPDATE snapshots SET status = $2, last_error = $3 WHERE id = $1;

-- name: DeleteSnapshotRow :exec
DELETE FROM snapshots WHERE id = $1;

-- name: ClaimVMRebuild :execrows
-- Claims an idle running or stopped VM for a rebuild and records the template and keys to rebuild with.
UPDATE vms SET busy = 'rebuilding', rebuild_template_id = $3, rebuild_keys = $4, rebuild_boot_script = $5
WHERE id = $1 AND user_id = $2 AND busy IS NULL AND state IN ('running', 'stopped') AND deleted_at IS NULL;

-- name: GetRebuildWork :one
SELECT v.id, v.host, v.state, v.busy, v.proxmox_vmid, v.hostname, v.ipv6, v.rebuild_template_id, v.rebuild_keys, v.rebuild_boot_script,
       p.vcpu, p.ram_mb, p.disk_gb,
       COALESCE(ht.proxmox_template_id, t.proxmox_template_id, 0)::int AS template_vmid, COALESCE(t.ci_user, 'root')::text AS ci_user,
       COALESCE(host(ip.address), '')::text AS ipv4, COALESCE(host(ip.gateway), '')::text AS gateway
FROM vms v
JOIN plans p ON p.id = v.plan_id
LEFT JOIN templates t ON t.id = v.rebuild_template_id
LEFT JOIN host_templates ht ON ht.template_id = t.id AND ht.host = v.host
LEFT JOIN ip_addresses ip ON ip.id = v.ipv4_id
WHERE v.id = $1;

-- name: FinishRebuild :execrows
-- The VM now runs the new template; its old snapshots went with the old disk.
UPDATE vms SET template_id = rebuild_template_id, authorized_keys = rebuild_keys, state = 'running',
       boot_script = rebuild_boot_script,
       boot_script_status = CASE WHEN rebuild_boot_script IS NULL THEN 'none' ELSE 'pending' END,
       boot_script_exit = NULL, boot_script_output = NULL,
       rebuild_template_id = NULL, rebuild_keys = NULL, rebuild_boot_script = NULL, busy = NULL
WHERE id = $1 AND busy = 'rebuilding' AND rebuild_template_id IS NOT NULL;

-- name: DeleteVMSnapshots :exec
DELETE FROM snapshots WHERE vm_id = $1;

-- name: MarkVMError :exec
UPDATE vms SET state = 'error', busy = NULL, rebuild_template_id = NULL, rebuild_keys = NULL, rebuild_boot_script = NULL WHERE id = $1;

-- name: GetBootScriptWork :one
SELECT id, state, proxmox_vmid, COALESCE(boot_script, '')::text AS boot_script, boot_script_status FROM vms WHERE id = $1;

-- name: ClaimBootScript :execrows
UPDATE vms SET boot_script_status = 'running' WHERE id = $1 AND boot_script_status = 'pending';

-- name: RevertBootScript :exec
UPDATE vms SET boot_script_status = 'pending' WHERE id = $1 AND boot_script_status = 'running';

-- name: FinishBootScript :exec
-- The script text is erased once it has run: it may carry a one-time registration token.
UPDATE vms SET boot_script_status = $2, boot_script_exit = $3, boot_script_output = $4, boot_script = NULL WHERE id = $1;

-- name: LockUserStatus :one
-- Takes the row lock that serialises creating VMs, closing the account and converting deposits.
SELECT status FROM users WHERE id = $1 FOR UPDATE;
