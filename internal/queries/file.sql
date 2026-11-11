-- name: CreateFile :one
INSERT INTO files (user_id, parent_id, name, storage_key, content_type, size_bytes)
VALUES ($1, $2, $3, $4, $5, $6)
    RETURNING *;

-- name: CreateFolder :one
INSERT INTO files (user_id, parent_id, name, content_type, size_bytes)
VALUES ($1, $2, $3, '', 0)
    RETURNING *;

-- name: FolderExists :one
SELECT EXISTS (
    SELECT 1 FROM files
    WHERE id = $1 AND user_id = $2 AND storage_key IS NULL
);

-- name: ListNamesByParentAndPrefix :many
SELECT name FROM files
WHERE user_id = sqlc.arg(user_id)
  AND COALESCE(parent_id, 0) = sqlc.arg(parent_id)::bigint
  AND starts_with(name, sqlc.arg(prefix)::text);

-- name: ListFilesByParentPaginated :many
SELECT * FROM files
WHERE user_id = sqlc.arg(user_id)
  AND COALESCE(parent_id, 0) = sqlc.arg(parent_id)::bigint
ORDER BY (storage_key IS NULL) DESC, created_at DESC, id DESC
    LIMIT sqlc.arg(row_limit) OFFSET sqlc.arg(row_offset);

-- name: CountFilesByParent :one
SELECT COUNT(*) FROM files
WHERE user_id = sqlc.arg(user_id)
  AND COALESCE(parent_id, 0) = sqlc.arg(parent_id)::bigint;

-- name: ListFolderPath :many
-- Returns the folder and its parents, root first. Empty when the folder is not one of the user's.
WITH RECURSIVE path AS (
    SELECT files.id, files.parent_id, files.name, 0 AS depth
    FROM files
    WHERE files.id = sqlc.arg(id) AND files.user_id = sqlc.arg(user_id) AND files.storage_key IS NULL
    UNION ALL
    SELECT f.id, f.parent_id, f.name, path.depth + 1
    FROM files f
    JOIN path ON f.id = path.parent_id
)
SELECT id, name FROM path
ORDER BY depth DESC;
