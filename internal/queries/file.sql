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

-- name: LockUserTree :exec
-- Makes one user's tree changes run one at a time. Each transaction that changes the tree runs it first.
SELECT pg_advisory_xact_lock(sqlc.arg(user_id)::bigint);

-- name: TrashItems :many
-- Marks only the items. Each item roots its own group. A job marks the rows under the folders (TrashChildren).
UPDATE files
SET trashed_at = NOW(), trash_root_id = id
WHERE user_id = sqlc.arg(user_id)
  AND id = ANY(sqlc.arg(ids)::bigint[])
  AND trashed_at IS NULL
    RETURNING id, (storage_key IS NULL)::boolean AS is_folder;

-- name: TrashChildren :execrows
-- Marks up to batch_size live rows right under parent_ids. Each row takes its parent's mark, and only a parent
-- still in one of the root_ids groups counts. So after a restore of a group, nothing is left to mark under it.
WITH batch AS (
    SELECT c.id, p.trashed_at, p.trash_root_id
    FROM files p
    JOIN files c ON c.parent_id = p.id
    WHERE p.user_id = sqlc.arg(user_id)
      AND p.id = ANY(sqlc.arg(parent_ids)::bigint[])
      AND p.trash_root_id = ANY(sqlc.arg(root_ids)::bigint[])
      AND c.trashed_at IS NULL
    LIMIT sqlc.arg(batch_size)
)
UPDATE files
SET trashed_at = batch.trashed_at, trash_root_id = batch.trash_root_id
FROM batch
WHERE files.id = batch.id;

-- name: ListGroupFolders :many
-- The folders right under parent_ids in one of the root_ids groups: the next level of the trash job.
-- It skips the group roots, because the job starts from them.
SELECT id FROM files
WHERE user_id = sqlc.arg(user_id)
  AND parent_id = ANY(sqlc.arg(parent_ids)::bigint[])
  AND trash_root_id = ANY(sqlc.arg(root_ids)::bigint[])
  AND trash_root_id <> id
  AND storage_key IS NULL;

-- name: ListTrashRoots :many
-- Locks the deleted items in ids. new_parent_id is the old folder if it is live or comes back with this restore, else 0 (Home).
-- An item marked for a permanent delete is left out.
SELECT f.id, f.name, (f.storage_key IS NULL)::boolean AS is_folder, COALESCE(p.id, 0)::bigint AS new_parent_id
FROM files f
LEFT JOIN files p ON p.id = f.parent_id
    AND (p.trashed_at IS NULL OR p.trash_root_id = ANY(sqlc.arg(ids)::bigint[]))
WHERE f.user_id = sqlc.arg(user_id)
  AND f.id = ANY(sqlc.arg(ids)::bigint[])
  AND f.trash_root_id = f.id
  AND f.purge_requested_at IS NULL
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

-- name: RequestPurge :execrows
-- Marks the deleted items in ids for a permanent delete. The trash view and restore skip a marked group.
UPDATE files
SET purge_requested_at = NOW()
WHERE user_id = sqlc.arg(user_id)
  AND id = ANY(sqlc.arg(ids)::bigint[])
  AND trash_root_id = id
  AND purge_requested_at IS NULL;

-- name: ListPurgeRoots :many
-- The items in ids that are marked for a permanent delete. A root the job already deleted is not in it.
SELECT id FROM files
WHERE user_id = sqlc.arg(user_id)
  AND id = ANY(sqlc.arg(ids)::bigint[])
  AND purge_requested_at IS NOT NULL
ORDER BY id;

-- name: MoveNestedTrashRootsHome :execrows
-- Moves up to batch_size items deleted earlier out of the root_ids groups, to Home. They stay in the trash.
-- Else the parent_id foreign key blocks the delete of the folder they were in.
WITH batch AS (
    SELECT c.id
    FROM files c
    JOIN files p ON p.id = c.parent_id
    WHERE c.user_id = sqlc.arg(user_id)
      AND c.trash_root_id = c.id
      AND p.trash_root_id = ANY(sqlc.arg(root_ids)::bigint[])
    LIMIT sqlc.arg(batch_size)
)
UPDATE files
SET parent_id = NULL
FROM batch
WHERE files.id = batch.id;

-- name: ListPurgeLeaves :many
-- Up to batch_size rows of the root_ids groups with no rows under them. The parent_id foreign key blocks
-- the delete of a folder that still has rows under it, so the purge deletes from the bottom up.
SELECT f.id, f.storage_key
FROM files f
WHERE f.user_id = sqlc.arg(user_id)
  AND f.trash_root_id = ANY(sqlc.arg(root_ids)::bigint[])
  AND NOT EXISTS (SELECT 1 FROM files c WHERE c.parent_id = f.id)
LIMIT sqlc.arg(batch_size);

-- name: DeletePurgeRows :exec
-- Deletes the rows in ids. A row outside the root_ids groups stays.
DELETE FROM files
WHERE user_id = sqlc.arg(user_id)
  AND id = ANY(sqlc.arg(ids)::bigint[])
  AND trash_root_id = ANY(sqlc.arg(root_ids)::bigint[]);

-- name: ListUsersWithExpiredTrash :many
-- The users with items in the trash longer than retention_days that are not marked for a permanent delete yet.
SELECT DISTINCT user_id FROM files
WHERE trash_root_id = id
  AND purge_requested_at IS NULL
  AND trashed_at < NOW() - make_interval(days => sqlc.arg(retention_days)::int)
ORDER BY user_id;

-- name: RequestExpiredPurge :many
-- Marks up to batch_size of the user's items in the trash longer than retention_days for a permanent delete.
WITH batch AS (
    SELECT f.id FROM files f
    WHERE f.user_id = sqlc.arg(user_id)
      AND f.trash_root_id = f.id
      AND f.purge_requested_at IS NULL
      AND f.trashed_at < NOW() - make_interval(days => sqlc.arg(retention_days)::int)
    LIMIT sqlc.arg(batch_size)
)
UPDATE files
SET purge_requested_at = NOW()
FROM batch
WHERE files.id = batch.id
RETURNING files.id;

-- name: ListStalePurges :many
-- The items marked for a permanent delete more than retry_hours ago, per user. Their purge job can have failed for good.
SELECT user_id, array_agg(id ORDER BY id)::bigint[] AS root_ids
FROM files
WHERE purge_requested_at < NOW() - make_interval(hours => sqlc.arg(retry_hours)::int)
GROUP BY user_id
ORDER BY user_id;
