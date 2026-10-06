-- name: AdminListUsers :many
SELECT u.id, u.email, u.status, u.is_admin, u.vm_limit, u.email_verified_at, u.created_at, u.ispend_customer_id,
       (SELECT count(*) FROM vms v WHERE v.user_id = u.id AND v.state NOT IN ('deleted', 'error'))::int AS vm_count
FROM users u
WHERE ($1::text = '' OR u.email ILIKE '%' || $1 || '%')
ORDER BY u.id DESC
LIMIT $2 OFFSET $3;

-- name: SetUserStatusByID :execrows
UPDATE users SET status = $2 WHERE id = $1;

-- name: SetUserVMLimit :execrows
UPDATE users SET vm_limit = $2 WHERE id = $1;

-- name: AdminListVMs :many
SELECT v.id, v.hostname, v.state, v.region, v.created_at, v.flagged_at, COALESCE(v.flag_reason, '')::text AS flag_reason,
       v.port25_unblocked, v.user_id, u.email, p.slug AS plan_slug, p.price_uusdt_hourly,
       COALESCE(host(ip.address), '')::text AS ipv4, COALESCE(v.ipv6, '')::text AS ipv6
FROM vms v
JOIN users u ON u.id = v.user_id
JOIN plans p ON p.id = v.plan_id
LEFT JOIN ip_addresses ip ON ip.id = v.ipv4_id
WHERE v.deleted_at IS NULL
  AND ($1::text = '' OR v.state = $1)
  AND ($2::text = '' OR u.email ILIKE '%' || $2 || '%' OR v.hostname ILIKE '%' || $2 || '%')
  AND (NOT $3::bool OR v.flagged_at IS NOT NULL)
ORDER BY v.id DESC
LIMIT 200;

-- name: CreateAdjustment :one
-- A repeat of (admin, request_id) inserts nothing and returns no row: the caller then reads the original.
INSERT INTO adjustments (admin_id, user_id, amount_uusdt, note, request_id) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (admin_id, request_id) WHERE request_id IS NOT NULL DO NOTHING
RETURNING id;

-- name: GetAdjustmentByRequest :one
SELECT id, user_id, amount_uusdt, note, status FROM adjustments WHERE admin_id = $1 AND request_id = $2;

-- name: SumAdminAdjustments24h :one
SELECT COALESCE(sum(abs(amount_uusdt)), 0)::bigint FROM adjustments
WHERE admin_id = $1 AND status <> 'failed' AND created_at > now() - interval '24 hours';

-- name: CompleteAdjustment :exec
UPDATE adjustments SET status = 'complete', ispend_movement_id = $2, last_error = NULL WHERE id = $1;

-- name: FailAdjustment :exec
UPDATE adjustments SET status = 'failed', last_error = $2 WHERE id = $1;

-- name: ListUserAdjustments :many
SELECT a.id, a.amount_uusdt, a.note, a.status, a.created_at, adm.email AS admin_email
FROM adjustments a JOIN users adm ON adm.id = a.admin_id
WHERE a.user_id = $1 ORDER BY a.id DESC LIMIT 20;

-- name: InsertAudit :exec
INSERT INTO admin_audit (admin_id, action, target, detail) VALUES ($1, $2, $3, $4);

-- name: ListJobsByStatus :many
SELECT id, kind, payload, status, attempts, COALESCE(last_error, '')::text AS last_error, created_at
FROM jobs WHERE status = $1 ORDER BY id DESC LIMIT 100;

-- name: RetryJob :execrows
UPDATE jobs SET status = 'queued', attempts = 0, run_after = now(), alerted_at = NULL WHERE id = $1 AND status = 'failed';

-- name: CommittedVCPU :one
SELECT COALESCE(sum(p.vcpu), 0)::bigint FROM vms v JOIN plans p ON p.id = v.plan_id
WHERE v.state IN ('pending', 'provisioning', 'running');

-- name: CountTotalIPs :one
SELECT count(*) FROM ip_addresses;

-- name: RevenueByDay :many
SELECT d::date AS day,
       COALESCE((SELECT sum(c.amount_ngn_kobo) FROM conversions c
                 WHERE c.status = 'complete' AND c.created_at >= d AND c.created_at < d + interval '1 day'), 0)::bigint AS converted_ngn_kobo,
       COALESCE((SELECT sum(c.amount_uusdt) FROM conversions c
                 WHERE c.status = 'complete' AND c.created_at >= d AND c.created_at < d + interval '1 day'), 0)::bigint AS converted_uusdt,
       COALESCE((SELECT sum(x.amount_uusdt) FROM usage_charges x
                 WHERE x.status = 'paid' AND x.hour >= d AND x.hour < d + interval '1 day'), 0)::bigint AS usage_uusdt
FROM generate_series($1::timestamptz, $2::timestamptz, interval '1 day') AS d
ORDER BY d DESC;
