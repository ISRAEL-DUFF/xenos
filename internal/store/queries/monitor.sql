-- name: ListUnalertedFailedJobs :many
SELECT id, kind, COALESCE(last_error, '')::text AS last_error FROM jobs
WHERE status = 'failed' AND alerted_at IS NULL ORDER BY id LIMIT 20;

-- name: MarkJobsAlerted :exec
UPDATE jobs SET alerted_at = $2 WHERE id = ANY($1::bigint[]);

-- name: CommittedRAMMB :one
SELECT COALESCE(sum(p.ram_mb), 0)::bigint FROM vms v JOIN plans p ON p.id = v.plan_id
WHERE v.state IN ('pending', 'provisioning', 'running');

-- name: CountFreeIPs :one
SELECT count(*) FROM ip_addresses WHERE vm_id IS NULL;

-- name: CountStalePendingCharges :one
SELECT count(*) FROM usage_charges WHERE status = 'pending' AND created_at < $1;

-- name: CountStuckConversions :one
SELECT count(*) FROM conversions
WHERE (status = 'failed' AND created_at > $1) OR (status = 'pending' AND last_error IS NOT NULL AND created_at < $2);

-- name: RecordWebhookFailure :exec
INSERT INTO webhook_failures (ip) VALUES ($1);

-- name: CountWebhookFailuresSince :one
SELECT count(*) FROM webhook_failures WHERE at >= $1;

-- name: PruneWebhookFailures :exec
DELETE FROM webhook_failures WHERE at < $1;

-- name: ClaimAlert :one
INSERT INTO alerts_sent (key, last_sent_at) VALUES ($1, $2)
ON CONFLICT (key) DO UPDATE SET last_sent_at = EXCLUDED.last_sent_at
  WHERE alerts_sent.last_sent_at <= $3
RETURNING key;

-- name: Heartbeat :exec
INSERT INTO heartbeats (name, at) VALUES ($1, $2)
ON CONFLICT (name) DO UPDATE SET at = EXCLUDED.at;

-- name: GetHeartbeat :one
SELECT at FROM heartbeats WHERE name = $1;

-- name: ListRunningVMsForCPU :many
SELECT id, proxmox_vmid, hostname, cpu_high_since, flagged_at FROM vms
WHERE state = 'running' AND proxmox_vmid IS NOT NULL;

-- name: SetCPUHighSince :exec
UPDATE vms SET cpu_high_since = $2 WHERE id = $1;

-- name: FlagVM :exec
UPDATE vms SET flagged_at = $2, flag_reason = $3 WHERE id = $1 AND flagged_at IS NULL;

-- name: ListFlaggedVMs :many
SELECT v.id, v.hostname, v.state, v.flagged_at, COALESCE(v.flag_reason, '')::text AS flag_reason, u.email
FROM vms v JOIN users u ON u.id = v.user_id
WHERE v.flagged_at IS NOT NULL AND v.deleted_at IS NULL ORDER BY v.flagged_at;

-- name: ClearVMFlag :exec
UPDATE vms SET flagged_at = NULL, flag_reason = NULL, cpu_high_since = NULL WHERE id = $1;

-- name: SetPort25 :execrows
UPDATE vms SET port25_unblocked = $2 WHERE id = $1 AND deleted_at IS NULL;

-- name: ListPort25Unblocked :many
SELECT COALESCE(host(ip.address), '')::text AS ipv4, COALESCE(v.ipv6, '')::text AS ipv6
FROM vms v LEFT JOIN ip_addresses ip ON ip.id = v.ipv4_id
WHERE v.port25_unblocked AND v.deleted_at IS NULL AND v.state NOT IN ('deleted', 'error');

-- name: BanUser :exec
UPDATE users SET status = 'banned' WHERE email = $1;

-- name: SetUserStatus :execrows
UPDATE users SET status = $2 WHERE email = $1;

-- name: ListUserLiveVMIDs :many
SELECT id FROM vms WHERE user_id = $1 AND state IN ('running', 'stopped');

-- name: MarkAUP :exec
UPDATE users SET aup_accepted_at = $2 WHERE id = $1;

-- name: ListRunningVMsToCheck :many
-- Running VMs that nothing is working on: not busy, and no power, suspend, delete or resize job queued, running or
-- finished in the last five minutes (a customer's own stop must not be undone).
SELECT v.id, v.host, v.proxmox_vmid, v.hostname FROM vms v
WHERE v.state = 'running' AND v.busy IS NULL AND v.proxmox_vmid IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM jobs j
                  WHERE j.kind IN ('vm.power', 'vm.suspend', 'vm.delete', 'vm.resize', 'vm.rebuild', 'vm.restore', 'vm.resume')
                    AND j.payload->>'vm_id' = v.id::text
                    AND (j.status IN ('queued', 'running') OR j.created_at > now() - interval '5 minutes'));
