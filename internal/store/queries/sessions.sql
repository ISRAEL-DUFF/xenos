-- name: CreateSession :exec
INSERT INTO sessions (token_hash, user_id, kind, csrf_token, expires_at)
VALUES ($1, $2, $3, $4, $5);

-- name: GetSessionUser :one
SELECT s.token_hash, s.kind, s.csrf_token, s.expires_at, u.*
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = $1 AND s.expires_at > now();

-- name: DeleteSession :exec
DELETE FROM sessions WHERE token_hash = $1;

-- name: DeleteUserSessions :exec
DELETE FROM sessions WHERE user_id = $1;

-- name: DeleteExpiredSessions :exec
DELETE FROM sessions WHERE expires_at <= now();
