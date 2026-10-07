-- name: CreateSSHKey :one
INSERT INTO ssh_keys (user_id, name, public_key, fingerprint)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListSSHKeys :many
SELECT * FROM ssh_keys WHERE user_id = $1 ORDER BY created_at DESC, id DESC;

-- name: CountSSHKeys :one
SELECT count(*) FROM ssh_keys WHERE user_id = $1;

-- name: DeleteSSHKey :execrows
DELETE FROM ssh_keys WHERE id = $1 AND user_id = $2;
