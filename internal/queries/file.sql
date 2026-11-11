-- name: CreateFile :one
INSERT INTO files (user_id, name, storage_key, content_type, size_bytes)
VALUES ($1, $2, $3, $4, $5)
    RETURNING *;
