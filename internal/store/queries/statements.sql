-- name: StatementMonths :many
-- UTC calendar months in which the account had a charge, a conversion or an adjustment, newest first.
SELECT t.month::text AS month FROM (
    SELECT to_char(c.hour AT TIME ZONE 'UTC', 'YYYY-MM') AS month FROM usage_charges c WHERE c.user_id = sqlc.arg(user_id)::bigint
    UNION SELECT to_char(v.created_at AT TIME ZONE 'UTC', 'YYYY-MM') FROM conversions v WHERE v.user_id = sqlc.arg(user_id)::bigint AND v.status = 'complete'
    UNION SELECT to_char(a.created_at AT TIME ZONE 'UTC', 'YYYY-MM') FROM adjustments a WHERE a.user_id = sqlc.arg(user_id)::bigint AND a.status = 'complete'
) t ORDER BY t.month DESC;

-- name: StatementCharges :many
-- One row per charged hour in [from, to), deleted VMs included.
SELECT v.id AS vm_id, v.hostname, v.labels, p.slug AS plan_slug, c.hour, c.amount_uusdt, c.status
FROM usage_charges c
JOIN vms v ON v.id = c.vm_id
JOIN plans p ON p.id = v.plan_id
WHERE c.user_id = $1 AND c.hour >= $2 AND c.hour < $3
ORDER BY c.vm_id, c.hour;

-- name: StatementConversions :many
SELECT id, amount_ngn_kobo, amount_uusdt, COALESCE(rate::text, '')::text AS rate, created_at
FROM conversions WHERE user_id = $1 AND status = 'complete' AND created_at >= $2 AND created_at < $3 ORDER BY id;

-- name: StatementAdjustments :many
SELECT id, amount_uusdt, note, created_at
FROM adjustments WHERE user_id = $1 AND status = 'complete' AND created_at >= $2 AND created_at < $3 ORDER BY id;

-- name: StatementUnpaid :one
SELECT COALESCE(sum(amount_uusdt), 0)::bigint FROM usage_charges WHERE user_id = $1 AND status = 'unpaid';
