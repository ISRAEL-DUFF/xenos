-- name: ListBillingVMs :many
SELECT v.id, v.user_id, v.billing_from, v.billing_until, v.created_at,
       p.price_uusdt_hourly, p.price_uusdt_monthly_cap
FROM vms v JOIN plans p ON p.id = v.plan_id
WHERE v.billing_from IS NOT NULL
ORDER BY v.id;

-- name: InsertUsageCharge :one
INSERT INTO usage_charges (user_id, vm_id, hour, amount_uusdt, status)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (vm_id, hour) DO NOTHING
RETURNING id;

-- name: SetBillingFrom :exec
UPDATE vms SET billing_from = $2 WHERE id = $1;

-- name: MonthChargedForVM :one
SELECT COALESCE(sum(amount_uusdt), 0)::bigint FROM usage_charges
WHERE vm_id = $1 AND hour >= $2 AND hour < $3 AND status <> 'refunded';

-- name: ListOpenCharges :many
SELECT c.id, c.user_id, c.vm_id, c.hour, c.amount_uusdt, c.status, u.ispend_customer_id
FROM usage_charges c JOIN users u ON u.id = c.user_id
WHERE c.status IN ('pending', 'unpaid')
ORDER BY c.hour, c.id
LIMIT 500;

-- name: MarkChargePaid :exec
UPDATE usage_charges SET status = 'paid', ispend_movement_id = $2 WHERE id = $1;

-- name: MarkChargeUnpaid :exec
UPDATE usage_charges SET status = 'unpaid' WHERE id = $1;

-- name: FinishBilling :exec
UPDATE vms SET billing_from = NULL
WHERE billing_from IS NOT NULL AND billing_until IS NOT NULL
  AND billing_from > date_trunc('hour', billing_until AT TIME ZONE 'UTC') AT TIME ZONE 'UTC';

-- name: StartGrace :execrows
UPDATE users SET grace_started_at = $2 WHERE id = $1 AND grace_started_at IS NULL;

-- name: ClearGrace :exec
UPDATE users SET grace_started_at = NULL WHERE id = $1;

-- name: ListGraceVMsToSuspend :many
SELECT v.id FROM vms v JOIN users u ON u.id = v.user_id
WHERE u.grace_started_at IS NOT NULL AND v.state IN ('running', 'stopped')
  AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.kind = 'vm.suspend' AND j.status IN ('queued', 'running')
                  AND j.payload->>'vm_id' = v.id::text);

-- name: ListGraceUsers :many
SELECT id, email, ispend_customer_id, grace_started_at FROM users WHERE grace_started_at IS NOT NULL;

-- name: ListUserSuspendedVMs :many
SELECT v.id, p.price_uusdt_hourly
FROM vms v JOIN plans p ON p.id = v.plan_id
WHERE v.user_id = $1 AND v.state = 'suspended';

-- name: ListUserVMsToDelete :many
SELECT v.id FROM vms v
WHERE v.user_id = $1 AND v.state IN ('suspended', 'running', 'stopped')
  AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.kind = 'vm.delete' AND j.status IN ('queued', 'running')
                  AND j.payload->>'vm_id' = v.id::text);

-- name: UserHasLiveVMs :one
SELECT EXISTS (SELECT 1 FROM vms WHERE user_id = $1
  AND state IN ('pending', 'provisioning', 'running', 'stopped', 'suspended', 'deleting'));

-- name: ListActiveBillingUsers :many
SELECT u.id, u.email, u.ispend_customer_id, u.low_balance_notified_at,
       sum(p.price_uusdt_hourly)::bigint AS hourly
FROM vms v JOIN plans p ON p.id = v.plan_id JOIN users u ON u.id = v.user_id
WHERE v.billing_from IS NOT NULL AND v.billing_until IS NULL AND u.grace_started_at IS NULL
GROUP BY u.id;

-- name: SetLowBalanceNotified :exec
UPDATE users SET low_balance_notified_at = $2 WHERE id = $1;

-- name: SuspendVM :execrows
UPDATE vms SET state = 'suspended', suspended_at = $2, billing_until = $2
WHERE id = $1 AND state IN ('running', 'stopped');

-- name: ResumeVM :execrows
UPDATE vms SET state = 'stopped', suspended_at = NULL, billing_from = $2, billing_until = NULL
WHERE id = $1 AND state = 'suspended';

-- name: MarkVMRunning :execrows
UPDATE vms SET state = 'running', billing_from = $2, billing_until = NULL
WHERE id = $1 AND state = 'provisioning';

-- name: StopBilling :exec
UPDATE vms SET billing_until = $2 WHERE id = $1 AND billing_from IS NOT NULL AND billing_until IS NULL;
