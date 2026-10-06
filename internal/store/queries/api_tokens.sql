-- name: CreateAPIToken :one
INSERT INTO api_tokens (user_id, name, prefix, token_hash, expires_at) VALUES ($1, $2, $3, $4, $5) RETURNING id;

-- name: ListAPITokens :many
SELECT id, name, prefix, created_at, last_used_at, expires_at
FROM api_tokens WHERE user_id = $1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now()) ORDER BY id DESC;

-- name: CountAPITokens :one
SELECT count(*) FROM api_tokens WHERE user_id = $1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now());

-- name: GetAPITokenUser :one
SELECT t.id AS token_id, t.token_hash, u.*
FROM api_tokens t JOIN users u ON u.id = t.user_id
WHERE t.token_hash = $1 AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at > now());

-- name: TouchAPIToken :exec
UPDATE api_tokens SET last_used_at = now() WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- name: RevokeAPIToken :execrows
UPDATE api_tokens SET revoked_at = now() WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL;

-- name: RevokeUserAPITokens :exec
UPDATE api_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL;
