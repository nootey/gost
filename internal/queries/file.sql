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
    WHERE id = $1 AND user_id = $2 AND storage_key IS NULL AND trashed_at IS NULL
);

-- name: GetFile :one
SELECT * FROM files
WHERE id = $1 AND user_id = $2 AND trashed_at IS NULL;

-- name: RenameFile :one
UPDATE files
SET name = sqlc.arg(name), updated_at = NOW()
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id) AND trashed_at IS NULL
    RETURNING *;

-- name: ListNamesByParentAndPrefix :many
SELECT name FROM files
WHERE user_id = sqlc.arg(user_id)
  AND COALESCE(parent_id, 0) = sqlc.arg(parent_id)::bigint
  AND starts_with(name, sqlc.arg(prefix)::text)
  AND trashed_at IS NULL;

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
      AND files.trashed_at IS NULL
    UNION ALL
    SELECT f.id, f.name, path.depth + 1
    FROM files f
    JOIN path ON COALESCE(f.parent_id, 0) = path.id
    WHERE f.user_id = sqlc.arg(user_id)
      AND f.name = (sqlc.arg(names)::text[])[path.depth + 1]
      AND f.storage_key IS NULL
      AND f.trashed_at IS NULL
)
SELECT id, name FROM path
ORDER BY depth;

-- name: TrashItems :one
-- Marks each item and the live rows under it. Each item roots its own group, so the walk stops at another item from the list.
-- Returns how many of the items it marked.
WITH RECURSIVE tree AS (
    SELECT files.id, files.id AS root_id
    FROM files
    WHERE files.user_id = sqlc.arg(user_id)
      AND files.id = ANY(sqlc.arg(ids)::bigint[])
      AND files.trashed_at IS NULL
    UNION ALL
    SELECT f.id, tree.root_id
    FROM files f
    JOIN tree ON COALESCE(f.parent_id, 0) = tree.id
    WHERE f.user_id = sqlc.arg(user_id)
      AND f.trashed_at IS NULL
      AND f.id <> ALL(sqlc.arg(ids)::bigint[])
),
trashed AS (
    UPDATE files
    SET trashed_at = NOW(), trash_root_id = tree.root_id
    FROM tree
    WHERE files.id = tree.id
    RETURNING files.id, files.trash_root_id
)
SELECT COUNT(*) FROM trashed
WHERE id = trash_root_id;

-- name: ListTrashRoots :many
-- Locks the deleted items in ids. new_parent_id is the old folder if it is live or comes back with this restore, else 0 (Home).
SELECT f.id, f.name, (f.storage_key IS NULL)::boolean AS is_folder, COALESCE(p.id, 0)::bigint AS new_parent_id
FROM files f
LEFT JOIN files p ON p.id = f.parent_id
    AND (p.trashed_at IS NULL OR p.trash_root_id = ANY(sqlc.arg(ids)::bigint[]))
WHERE f.user_id = sqlc.arg(user_id)
  AND f.id = ANY(sqlc.arg(ids)::bigint[])
  AND f.trash_root_id = f.id
ORDER BY f.id
FOR UPDATE OF f;

-- name: RestoreTrashGroups :exec
-- Brings back the rows under each deleted item. They keep their names, because nothing new can go into a folder in the trash.
UPDATE files
SET trashed_at = NULL, trash_root_id = NULL
WHERE user_id = sqlc.arg(user_id)
  AND trash_root_id = ANY(sqlc.arg(ids)::bigint[])
  AND trash_root_id <> id;

-- name: ListNamesByParentsAndPrefixes :many
-- The same as ListNamesByParentAndPrefix, for many folders at once. A name that matches another folder's prefix is still taken.
SELECT COALESCE(parent_id, 0)::bigint AS parent_id, name
FROM files
WHERE user_id = sqlc.arg(user_id)
  AND COALESCE(parent_id, 0) = ANY(sqlc.arg(parent_ids)::bigint[])
  AND name ^@ ANY(sqlc.arg(prefixes)::text[])
  AND trashed_at IS NULL;

-- name: RestoreTrashRoots :exec
-- parent_ids[i] and names[i] belong to ids[i].
UPDATE files
SET parent_id = NULLIF((sqlc.arg(parent_ids)::bigint[])[array_position(sqlc.arg(ids)::bigint[], id)], 0),
    name = (sqlc.arg(names)::text[])[array_position(sqlc.arg(ids)::bigint[], id)],
    trashed_at = NULL, trash_root_id = NULL, updated_at = NOW()
WHERE user_id = sqlc.arg(user_id)
  AND id = ANY(sqlc.arg(ids)::bigint[]);
