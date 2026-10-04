-- name: GetUserByISpendCustomer :one
SELECT * FROM users WHERE ispend_customer_id = $1;

-- name: UpdateAutoConvert :exec
UPDATE users SET auto_convert = $2 WHERE id = $1;

-- name: InsertWebhookEvent :execrows
INSERT INTO webhook_events (id) VALUES ($1) ON CONFLICT (id) DO NOTHING;

-- name: CreateConversion :one
INSERT INTO conversions (user_id, amount_ngn_kobo, deposit_event_id, ispend_quote_id)
VALUES ($1, $2, $3, $4)
ON CONFLICT (deposit_event_id) DO NOTHING
RETURNING id;

-- name: GetConversion :one
SELECT * FROM conversions WHERE id = $1;

-- name: GetConversionByKey :one
SELECT * FROM conversions WHERE deposit_event_id = $1;

-- name: CompleteConversion :exec
UPDATE conversions
SET status = 'complete', amount_uusdt = $2, rate = NULLIF($3::text, '')::numeric, ispend_quote_id = $4, ispend_movement_id = $5, last_error = NULL
WHERE id = $1;

-- name: SetConversionError :exec
UPDATE conversions SET last_error = $2 WHERE id = $1;

-- name: FailConversion :exec
UPDATE conversions SET status = 'failed', last_error = $2 WHERE id = $1;

-- name: ListUserConversions :many
SELECT id, amount_ngn_kobo, amount_uusdt, COALESCE(rate::text, '')::text AS rate, status, created_at
FROM conversions WHERE user_id = $1 ORDER BY id DESC LIMIT 10;

-- name: ListUserCharges :many
SELECT c.id, c.vm_id, v.hostname, c.hour, c.amount_uusdt, c.status
FROM usage_charges c JOIN vms v ON v.id = c.vm_id
WHERE c.user_id = $1 ORDER BY c.hour DESC, c.id DESC LIMIT 24;

-- name: UserHourlyRate :one
SELECT COALESCE(sum(p.price_uusdt_hourly), 0)::bigint
FROM vms v JOIN plans p ON p.id = v.plan_id
WHERE v.user_id = $1 AND v.billing_from IS NOT NULL AND v.billing_until IS NULL;

-- name: UserUnpaidTotal :one
SELECT COALESCE(sum(amount_uusdt), 0)::bigint FROM usage_charges
WHERE user_id = $1 AND status IN ('pending', 'unpaid');

-- name: SaveQuote :exec
INSERT INTO conversion_quotes (id, user_id, amount_ngn_kobo, amount_uusdt, rate)
VALUES ($1, $2, $3, $4, $5) ON CONFLICT (id) DO NOTHING;

-- name: GetUserQuote :one
SELECT * FROM conversion_quotes WHERE id = $1 AND user_id = $2;

-- name: ListStalledConversions :many
SELECT c.id FROM conversions c
WHERE c.status = 'pending'
  AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.kind = 'conversion.run' AND j.status IN ('queued', 'running')
                  AND j.payload->>'conversion_id' = c.id::text);
