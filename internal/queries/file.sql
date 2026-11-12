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

-- name: GetFile :one
SELECT * FROM files
WHERE id = $1 AND user_id = $2;

-- name: RenameFile :one
UPDATE files
SET name = sqlc.arg(name), updated_at = NOW()
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id)
    RETURNING *;

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

-- name: ResolveFolderPath :many
-- Walks folder names down from the root and returns each folder it finds, root first.
-- Fewer rows than names means the path does not exist.
WITH RECURSIVE path AS (
    SELECT files.id, files.name, 1 AS depth
    FROM files
    WHERE files.user_id = sqlc.arg(user_id)
      AND COALESCE(files.parent_id, 0) = 0
      AND files.name = (sqlc.arg(names)::text[])[1]
      AND files.storage_key IS NULL
    UNION ALL
    SELECT f.id, f.name, path.depth + 1
    FROM files f
    JOIN path ON COALESCE(f.parent_id, 0) = path.id
    WHERE f.user_id = sqlc.arg(user_id)
      AND f.name = (sqlc.arg(names)::text[])[path.depth + 1]
      AND f.storage_key IS NULL
)
SELECT id, name FROM path
ORDER BY depth;
