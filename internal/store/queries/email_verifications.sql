-- name: CreateEmailVerification :exec
INSERT INTO email_verifications (token_hash, user_id, expires_at) VALUES ($1, $2, $3);

-- name: ConsumeEmailVerification :one
DELETE FROM email_verifications
WHERE token_hash = $1 AND expires_at > now()
RETURNING user_id;

-- name: DeleteUserEmailVerifications :exec
DELETE FROM email_verifications WHERE user_id = $1;
