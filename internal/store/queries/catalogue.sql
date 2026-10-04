-- name: ListActivePlans :many
SELECT id, slug, vcpu, ram_mb, disk_gb, price_uusdt_hourly, price_uusdt_monthly_cap
FROM plans WHERE active ORDER BY price_uusdt_hourly;

-- name: ListActiveTemplates :many
SELECT id, slug, name FROM templates WHERE active ORDER BY id;
