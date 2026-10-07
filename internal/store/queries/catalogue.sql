-- name: ListActivePlans :many
SELECT id, slug, vcpu, ram_mb, disk_gb, price_uusdt_hourly, price_uusdt_monthly_cap
FROM plans WHERE active ORDER BY price_uusdt_hourly;

-- name: ListActiveTemplates :many
SELECT id, slug, name FROM templates WHERE active ORDER BY id;

-- name: AdminListPlans :many
-- Every plan, active or not, with how many live VMs sit on it and how many are being billed.
SELECT p.id, p.slug, p.vcpu, p.ram_mb, p.disk_gb, p.price_uusdt_hourly, p.price_uusdt_monthly_cap, p.active,
       (SELECT count(*) FROM vms v WHERE v.plan_id = p.id AND v.deleted_at IS NULL AND v.state <> 'error')::bigint AS vm_count
FROM plans p ORDER BY p.price_uusdt_hourly, p.id;

-- name: GetPlanAny :one
SELECT * FROM plans WHERE slug = $1;

-- name: InsertPlan :one
INSERT INTO plans (slug, vcpu, ram_mb, disk_gb, price_uusdt_hourly, price_uusdt_monthly_cap, active)
VALUES ($1, $2, $3, $4, $5, $6, TRUE) RETURNING *;

-- name: SetPlanPrice :exec
UPDATE plans SET price_uusdt_hourly = $2, price_uusdt_monthly_cap = $3 WHERE id = $1;

-- name: SetPlanActive :exec
UPDATE plans SET active = $2 WHERE id = $1;

-- name: CountBillingVMsOnPlan :one
-- VMs whose next hourly charge uses this plan's price.
SELECT count(*) FROM vms WHERE plan_id = $1 AND billing_from IS NOT NULL AND billing_until IS NULL;

-- name: AdminListTemplates :many
SELECT t.id, t.slug, t.name, t.proxmox_template_id, t.ci_user, t.active,
       (SELECT count(*) FROM vms v WHERE v.template_id = t.id AND v.deleted_at IS NULL)::bigint AS vm_count
FROM templates t ORDER BY t.id;

-- name: GetTemplateAny :one
SELECT * FROM templates WHERE slug = $1;

-- name: InsertTemplate :one
INSERT INTO templates (slug, name, proxmox_template_id, ci_user, active) VALUES ($1, $2, $3, $4, TRUE) RETURNING *;

-- name: SetTemplateActive :exec
UPDATE templates SET active = $2 WHERE id = $1;
