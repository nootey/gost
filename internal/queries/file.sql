-- name: CreateFile :one
INSERT INTO files (user_id, name, storage_key, content_type, size_bytes)
VALUES ($1, $2, $3, $4, $5)
    RETURNING *;

-- name: ListFilesByUserPaginated :many
SELECT * FROM files
WHERE user_id = $1
ORDER BY created_at DESC, id DESC
    LIMIT $2 OFFSET $3;

-- name: CountFilesByUser :one
SELECT COUNT(*) FROM files
WHERE user_id = $1;
