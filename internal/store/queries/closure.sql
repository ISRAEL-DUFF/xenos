-- name: BeginClosure :execrows
-- Moves an account into its 30-day closing period. Only an account that is not already closing or closed.
UPDATE users SET status = 'closing', closing_at = now(), purge_after = now() + interval '30 days', auto_convert = FALSE
WHERE id = $1 AND status IN ('active', 'suspended');

-- name: ReopenAccount :execrows
UPDATE users SET status = 'active', closing_at = NULL, purge_after = NULL
WHERE id = $1 AND status = 'closing';

-- name: ListClosingDue :many
SELECT id, email FROM users WHERE status = 'closing' AND purge_after <= $1 ORDER BY purge_after LIMIT 50;

-- name: AnonymiseClosedUser :exec
UPDATE users SET email = 'closed-' || id || '@invalid', phone = '', password_hash = '!', status = 'closed', closed_at = now(),
       auto_convert = FALSE, email_verified_at = NULL, va_account_name = NULL
WHERE id = $1 AND status = 'closing';

-- name: ScrubClosedUserVMs :exec
-- Hostnames, labels, keys and script output can identify a person; the rows stay for the charges that point at them.
UPDATE vms SET hostname = 'deleted-' || id, labels = '{}', authorized_keys = '', boot_script = NULL, boot_script_output = NULL,
       rebuild_keys = NULL, rebuild_boot_script = NULL
WHERE user_id = $1;

-- name: DeleteUserSSHKeys :exec
DELETE FROM ssh_keys WHERE user_id = $1;

-- name: DeleteUserVerificationRows :exec
DELETE FROM email_verifications WHERE user_id = $1;

-- name: DeleteUserResetRows :exec
DELETE FROM password_resets WHERE user_id = $1;

-- name: ScrubAuditTarget :exec
-- The audit log names accounts by email; after closure it names the anonymised address instead.
UPDATE admin_audit SET target = $2 WHERE target = $1;

-- name: ListClosedBefore :many
SELECT id, closed_at FROM users WHERE status = 'closed' AND closed_at < $1 ORDER BY closed_at;

-- name: ExportVMs :many
SELECT v.id, v.hostname, v.region, p.slug AS plan_slug, t.slug AS template_slug, v.state, v.labels,
       COALESCE(host(ip.address), '')::text AS ipv4, COALESCE(v.ipv6, '')::text AS ipv6, v.created_at, v.deleted_at
FROM vms v JOIN plans p ON p.id = v.plan_id JOIN templates t ON t.id = v.template_id
LEFT JOIN ip_addresses ip ON ip.id = v.ipv4_id
WHERE v.user_id = $1 ORDER BY v.id;

-- name: ExportCharges :many
SELECT c.id, c.vm_id, c.hour, c.amount_uusdt, c.status FROM usage_charges c WHERE c.user_id = $1 ORDER BY c.hour, c.id;

-- name: ExportConversions :many
SELECT id, amount_ngn_kobo, amount_uusdt, COALESCE(rate::text, '')::text AS rate, status, created_at
FROM conversions WHERE user_id = $1 ORDER BY id;

-- name: ExportAdjustments :many
SELECT id, amount_uusdt, note, status, created_at FROM adjustments WHERE user_id = $1 ORDER BY id;

-- name: ExportTokens :many
SELECT name, prefix, created_at, last_used_at, expires_at, revoked_at FROM api_tokens WHERE user_id = $1 ORDER BY id;

-- name: ExportSSHKeys :many
SELECT name, fingerprint, public_key, created_at FROM ssh_keys WHERE user_id = $1 ORDER BY id;

-- name: DeleteUserAPITokens :exec
DELETE FROM api_tokens WHERE user_id = $1;

-- name: ListUserDeletableVMs :many
-- Every VM of the account that still exists on the host or is being built: closing deletes all of them.
SELECT id FROM vms WHERE user_id = $1 AND state IN ('pending', 'provisioning', 'running', 'stopped', 'suspended', 'error');
