-- name: AddFloatingIP :exec
INSERT INTO floating_ips (address, region) VALUES ($1::inet, $2) ON CONFLICT (address) DO NOTHING;

-- name: ListFloatingPool :many
SELECT f.id, host(f.address)::text AS address, f.region, f.user_id, f.vm_id, f.label
FROM floating_ips f ORDER BY f.id;

-- name: ClaimFloatingIP :one
-- Takes the lowest free address of the region for the account and starts its billing at the top of the hour.
UPDATE floating_ips SET user_id = sqlc.arg(user_id), label = sqlc.arg(label), allocated_at = now(),
       billing_user_id = sqlc.arg(user_id), billing_from = sqlc.arg(billing_from), billing_until = NULL
WHERE floating_ips.id = (SELECT p.id FROM floating_ips p WHERE p.region = sqlc.arg(want_region) AND p.user_id IS NULL AND p.billing_from IS NULL AND p.applied_vm_id IS NULL
            ORDER BY p.id LIMIT 1 FOR UPDATE SKIP LOCKED)
RETURNING id, host(address)::text AS address, region, user_id, vm_id, label, allocated_at;

-- name: GetUserFloatingIP :one
SELECT id, host(address)::text AS address, region, user_id, vm_id, applied_vm_id, label, allocated_at
FROM floating_ips WHERE id = $1 AND user_id = $2;

-- name: ListUserFloatingIPs :many
SELECT id, host(address)::text AS address, region, user_id, vm_id, applied_vm_id, label, allocated_at
FROM floating_ips WHERE user_id = $1 ORDER BY id;

-- name: CountUserFloatingIPs :one
SELECT count(*) FROM floating_ips WHERE user_id = $1;

-- name: SetFloatingTarget :execrows
UPDATE floating_ips SET vm_id = $3 WHERE id = $1 AND user_id = $2;

-- name: ReleaseFloatingIP :execrows
-- Gives the address back to the pool once the worker has taken it off its VM; billing stops at the end of the
-- current hour (the last hour is already charged in advance).
UPDATE floating_ips SET user_id = NULL, vm_id = NULL, label = '', billing_until = $3
WHERE id = $1 AND user_id = $2;

-- name: LockFloatingIP :one
SELECT id FROM floating_ips WHERE id = $1 FOR UPDATE;

-- name: GetFloatingForWork :one
SELECT id, host(address)::text AS address, user_id, vm_id, applied_vm_id FROM floating_ips WHERE id = $1;

-- name: MarkFloatingApplied :exec
UPDATE floating_ips SET applied_vm_id = $2, applied_at = now() WHERE id = $1;

-- name: FloatingForVM :many
-- Addresses wanted on this VM (what its firewall set and interface should hold).
SELECT id, host(address)::text AS address FROM floating_ips WHERE vm_id = $1 ORDER BY id;

-- name: DetachFloatingFromVM :exec
-- A deleted VM takes its floating IPs with it only in the sense that they stop pointing at it; the account keeps (and pays for) them.
UPDATE floating_ips SET vm_id = NULL WHERE vm_id = $1;

-- name: ClearFloatingApplied :exec
UPDATE floating_ips SET applied_vm_id = NULL WHERE applied_vm_id = $1;

-- name: FloatingDueForCheck :many
-- Attached addresses whose VM is running and that have not been confirmed on the guest for an hour.
SELECT f.id FROM floating_ips f JOIN vms v ON v.id = f.vm_id
WHERE f.vm_id IS NOT NULL AND v.state = 'running' AND (f.applied_vm_id IS DISTINCT FROM f.vm_id OR f.applied_at IS NULL OR f.applied_at < $1)
  AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.kind = 'vm.floating' AND j.status IN ('queued', 'running') AND j.payload->>'floating_id' = f.id::text)
ORDER BY f.id LIMIT 100;

-- name: ListBillingFloating :many
SELECT id, billing_user_id AS user_id, billing_from, billing_until
FROM floating_ips WHERE billing_from IS NOT NULL AND billing_user_id IS NOT NULL ORDER BY id;

-- name: SetFloatingBillingFrom :exec
UPDATE floating_ips SET billing_from = $2 WHERE id = $1;

-- name: FinishFloatingBilling :exec
UPDATE floating_ips SET billing_from = NULL, billing_user_id = NULL
WHERE billing_from IS NOT NULL AND billing_until IS NOT NULL
  AND billing_from > date_trunc('hour', billing_until AT TIME ZONE 'UTC') AT TIME ZONE 'UTC';

-- name: InsertFloatingCharge :one
INSERT INTO usage_charges (user_id, floating_ip_id, hour, amount_uusdt, status)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (floating_ip_id, hour) WHERE floating_ip_id IS NOT NULL DO NOTHING
RETURNING id;

-- name: StatementFloatingCharges :many
SELECT c.floating_ip_id, host(f.address)::text AS address, c.hour, c.amount_uusdt, c.status
FROM usage_charges c JOIN floating_ips f ON f.id = c.floating_ip_id
WHERE c.user_id = $1 AND c.hour >= $2 AND c.hour < $3
ORDER BY c.floating_ip_id, c.hour;
