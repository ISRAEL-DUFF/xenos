-- name: CreatePasswordReset :exec
INSERT INTO password_resets (token_hash, user_id, expires_at) VALUES ($1, $2, $3);

-- name: ConsumePasswordReset :one
DELETE FROM password_resets
WHERE token_hash = $1 AND expires_at > now()
RETURNING user_id;

-- name: DeleteUserPasswordResets :exec
DELETE FROM password_resets WHERE user_id = $1;
