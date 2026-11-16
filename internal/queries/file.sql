-- name: CreateFile :one
INSERT INTO files (user_id, parent_id, name, storage_key, content_type, size_bytes)
VALUES ($1, $2, $3, $4, $5, $6)
    RETURNING *;

-- name: CreateFolder :one
INSERT INTO files (user_id, parent_id, name, content_type, size_bytes)
VALUES ($1, $2, $3, '', 0)
    RETURNING *;

-- name: NextFileIDs :many
-- Takes count new file IDs, so one insert can add folders together with the folders under them.
SELECT nextval(pg_get_serial_sequence('files', 'id'))::bigint AS id
FROM generate_series(1, sqlc.arg(count)::bigint);

-- name: CreateFolders :exec
-- Adds many folders in one statement, with IDs from NextFileIDs. The arrays hold one row each at the same index.
-- A parent ID of 0 is the root.
INSERT INTO files (id, user_id, parent_id, name, content_type, size_bytes)
SELECT t.id, sqlc.arg(user_id)::bigint, NULLIF(t.parent_id, 0), t.name, '', 0
FROM (
    SELECT unnest(sqlc.arg(ids)::bigint[]) AS id,
           unnest(sqlc.arg(parent_ids)::bigint[]) AS parent_id,
           unnest(sqlc.arg(names)::text[]) AS name
) AS t;

-- name: FolderExists :one
-- A folder that still copies does not count: nothing goes into it until the copy job ends.
SELECT EXISTS (
    SELECT 1 FROM files
    WHERE id = $1 AND user_id = $2 AND storage_key IS NULL AND trashed_at IS NULL AND copy_requested_at IS NULL
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
-- Fewer rows than names means the path does not exist. The walk stops at a folder that still copies.
WITH RECURSIVE path AS (
    SELECT files.id, files.name, 1 AS depth
    FROM files
    WHERE files.user_id = sqlc.arg(user_id)
      AND COALESCE(files.parent_id, 0) = 0
      AND files.name = (sqlc.arg(names)::text[])[1]
      AND files.storage_key IS NULL
      AND files.trashed_at IS NULL
      AND files.copy_requested_at IS NULL
    UNION ALL
    SELECT f.id, f.name, path.depth + 1
    FROM files f
    JOIN path ON COALESCE(f.parent_id, 0) = path.id
    WHERE f.user_id = sqlc.arg(user_id)
      AND f.name = (sqlc.arg(names)::text[])[path.depth + 1]
      AND f.storage_key IS NULL
      AND f.trashed_at IS NULL
      AND f.copy_requested_at IS NULL
)
SELECT id, name FROM path
ORDER BY depth;

-- name: LockUserTree :exec
-- Makes one user's tree changes run one at a time. Each transaction that changes the tree runs it first.
SELECT pg_advisory_xact_lock(sqlc.arg(user_id)::bigint);

-- name: TrashItems :many
-- Marks only the items. Each item roots its own group. A job marks the rows under the folders (TrashChildren).
-- It marks an item that still copies too, and the caller undoes the delete.
UPDATE files
SET trashed_at = NOW(), trash_root_id = id
WHERE user_id = sqlc.arg(user_id)
  AND id = ANY(sqlc.arg(ids)::bigint[])
  AND trashed_at IS NULL
    RETURNING id, name, (storage_key IS NULL)::boolean AS is_folder, (copy_requested_at IS NOT NULL)::boolean AS copying;

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

-- name: DeletePurgeRows :execrows
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

-- name: HasStoredFiles :one
-- Reports whether any row points to bytes on disk.
SELECT EXISTS (SELECT 1 FROM files WHERE storage_key IS NOT NULL);

-- name: ListOrphanKeys :many
-- The names in keys that no row has as its storage key.
SELECT k.key::text FROM unnest(sqlc.arg(keys)::text[]) AS k(key)
WHERE NOT EXISTS (SELECT 1 FROM files f WHERE f.storage_key = k.key);

-- name: LockLiveItems :many
-- Locks the live items in ids, so a rename cannot change a name before a move or a copy checks it.
SELECT id, COALESCE(parent_id, 0)::bigint AS parent_id, name,
       (storage_key IS NULL)::boolean AS is_folder, (copy_requested_at IS NOT NULL)::boolean AS copying
FROM files
WHERE user_id = sqlc.arg(user_id)
  AND id = ANY(sqlc.arg(ids)::bigint[])
  AND trashed_at IS NULL
ORDER BY id
FOR UPDATE;

-- name: ListFolderPathIDs :many
-- The folder and every folder above it, up to the root. No rows means the folder is not a live folder of the user,
-- or it still copies. UNION, not UNION ALL, so a loop in the data cannot make the walk run forever.
WITH RECURSIVE up AS (
    SELECT files.id, files.parent_id
    FROM files
    WHERE files.id = sqlc.arg(id)
      AND files.user_id = sqlc.arg(user_id)
      AND files.storage_key IS NULL
      AND files.trashed_at IS NULL
      AND files.copy_requested_at IS NULL
    UNION
    SELECT f.id, f.parent_id
    FROM files f
    JOIN up ON f.id = up.parent_id
)
SELECT id FROM up;

-- name: ListFolderPaths :many
-- The path of each folder in ids: the folder names from the root down to the folder. A repeated id gives one row.
-- The walk keeps the ids it passed, so a loop in the data ends the walk, and that folder gets no row.
WITH RECURSIVE up AS (
    SELECT files.id AS folder_id, files.parent_id, ARRAY[files.name]::text[] AS names, ARRAY[files.id]::bigint[] AS seen
    FROM files
    WHERE files.user_id = sqlc.arg(user_id)
      AND files.id = ANY(sqlc.arg(ids)::bigint[])
    UNION ALL
    SELECT up.folder_id, f.parent_id, array_prepend(f.name, up.names), array_prepend(f.id, up.seen)
    FROM files f
    JOIN up ON f.id = up.parent_id
    WHERE f.id <> ALL(up.seen)
)
SELECT folder_id::bigint AS folder_id, names::text[] AS names FROM up
WHERE parent_id IS NULL;

-- name: ListFolderSizes :many
-- The total size of the files at any depth under each folder in ids. Hidden files count. A live folder counts its live
-- rows. A folder in the trash counts the rows deleted with it, and the rows the trash job has not marked yet. A row in
-- another trash group is gone from the folder, so it does not count, and the walk does not go under it. UNION drops a
-- row it has already seen, so a loop in the data ends the walk.
WITH RECURSIVE down AS (
    SELECT files.id AS folder_id, files.id, files.trash_root_id AS group_id, 0::bigint AS size_bytes
    FROM files
    WHERE files.user_id = sqlc.arg(user_id)
      AND files.id = ANY(sqlc.arg(ids)::bigint[])
      AND files.storage_key IS NULL
    UNION
    SELECT down.folder_id, f.id, down.group_id, f.size_bytes
    FROM files f
    JOIN down ON f.parent_id = down.id
    WHERE f.trashed_at IS NULL OR f.trash_root_id = down.group_id
)
SELECT folder_id::bigint AS folder_id, SUM(size_bytes)::bigint AS size_bytes FROM down
GROUP BY folder_id;

-- name: ListTakenNames :many
-- The names in names that a live item in the folder already has.
SELECT name FROM files
WHERE user_id = sqlc.arg(user_id)
  AND COALESCE(parent_id, 0) = sqlc.arg(parent_id)::bigint
  AND name = ANY(sqlc.arg(names)::text[])
  AND trashed_at IS NULL
ORDER BY name;

-- name: MoveItems :exec
-- A parent_id of 0 means the root. The rows under a folder keep their parent_id, so they move with it.
UPDATE files
SET parent_id = NULLIF(sqlc.arg(parent_id)::bigint, 0), updated_at = NOW()
WHERE user_id = sqlc.arg(user_id)
  AND id = ANY(sqlc.arg(ids)::bigint[])
  AND trashed_at IS NULL;

-- name: CreateCopyRoots :many
-- Adds a copy of each live item in ids to the folder, marked as copying. keys[i] is the new storage key of ids[i],
-- empty for a folder. A parent_id of 0 means the root.
INSERT INTO files (user_id, parent_id, name, storage_key, content_type, size_bytes, copied_from_id, copy_requested_at, hidden_at)
SELECT src.user_id, NULLIF(sqlc.arg(parent_id)::bigint, 0), src.name,
       NULLIF((sqlc.arg(keys)::text[])[array_position(sqlc.arg(ids)::bigint[], src.id)], ''),
       src.content_type, src.size_bytes, src.id, NOW(), src.hidden_at
FROM files src
WHERE src.user_id = sqlc.arg(user_id)
  AND src.id = ANY(sqlc.arg(ids)::bigint[])
  AND src.trashed_at IS NULL
    RETURNING id;

-- name: ListCopyRoots :many
-- The copy roots in ids that still copy, with the storage key of their source. A root the job finished is not in it.
SELECT c.id, (c.storage_key IS NULL)::boolean AS is_folder, c.storage_key,
       COALESCE(c.copied_from_id, 0)::bigint AS source_id, s.storage_key AS source_key
FROM files c
LEFT JOIN files s ON s.id = c.copied_from_id AND s.user_id = c.user_id
WHERE c.user_id = sqlc.arg(user_id)
  AND c.id = ANY(sqlc.arg(ids)::bigint[])
  AND c.copy_requested_at IS NOT NULL
ORDER BY c.id;

-- name: ListUncopiedChildren :many
-- Up to batch_size live rows right under the sources of the copy folders in copy_ids that have no copy in them yet.
-- A copy folder in the trash gets nothing more. A copy that is not done yet is left out, and so are the rows in skip_ids.
SELECT p.id AS copy_parent_id, s.id, s.name, s.storage_key, s.content_type, s.size_bytes
FROM files p
JOIN files s ON s.parent_id = p.copied_from_id
WHERE p.user_id = sqlc.arg(user_id)
  AND p.id = ANY(sqlc.arg(copy_ids)::bigint[])
  AND p.storage_key IS NULL
  AND p.trashed_at IS NULL
  AND s.user_id = sqlc.arg(user_id)
  AND s.trashed_at IS NULL
  AND s.copy_requested_at IS NULL
  AND s.id <> ALL(sqlc.arg(skip_ids)::bigint[])
  AND NOT EXISTS (SELECT 1 FROM files c WHERE c.copied_from_id = s.id AND c.parent_id = p.id)
LIMIT sqlc.arg(batch_size);

-- name: ListTakenNamesInFolders :many
-- The live items in the folders whose name is one of names. The caller pairs each folder with its own names.
-- COALESCE matches the unique name index, so each folder and name is one index lookup.
SELECT COALESCE(parent_id, 0)::bigint AS parent_id, name FROM files
WHERE user_id = sqlc.arg(user_id)
  AND COALESCE(parent_id, 0) = ANY(sqlc.arg(parent_ids)::bigint[])
  AND name = ANY(sqlc.arg(names)::text[])
  AND trashed_at IS NULL;

-- name: InsertCopies :many
-- Adds one batch of copies. The arrays hold one row each at the same index, and the unnest calls read them in step.
-- An empty key marks a folder. A row whose copy folder is gone or in the trash is left out, and so is a row whose
-- copy is in the folder already: another run of the job added it. It returns the storage keys of the rows it added.
-- A copy keeps the hidden mark of its source.
INSERT INTO files (user_id, parent_id, name, storage_key, content_type, size_bytes, copied_from_id, hidden_at)
SELECT p.user_id, p.id, u.name, NULLIF(u.storage_key, ''), u.content_type, u.size_bytes, u.copied_from_id, src.hidden_at
FROM (
    SELECT unnest(sqlc.arg(parent_ids)::bigint[]) AS parent_id,
           unnest(sqlc.arg(names)::text[]) AS name,
           unnest(sqlc.arg(keys)::text[]) AS storage_key,
           unnest(sqlc.arg(content_types)::text[]) AS content_type,
           unnest(sqlc.arg(sizes)::bigint[]) AS size_bytes,
           unnest(sqlc.arg(source_ids)::bigint[]) AS copied_from_id
) AS u
JOIN files p ON p.id = u.parent_id
LEFT JOIN files src ON src.id = u.copied_from_id
WHERE p.user_id = sqlc.arg(user_id)
  AND p.trashed_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM files c WHERE c.copied_from_id = u.copied_from_id AND c.parent_id = p.id)
    RETURNING storage_key;

-- name: ListCopyFolders :many
-- The folders the copy job made right under parent_ids: the next level of the job.
SELECT id FROM files
WHERE user_id = sqlc.arg(user_id)
  AND parent_id = ANY(sqlc.arg(parent_ids)::bigint[])
  AND copied_from_id IS NOT NULL
  AND storage_key IS NULL
  AND trashed_at IS NULL;

-- name: DeleteCopyRoot :exec
-- Deletes a file copy root whose source has no bytes left to copy.
DELETE FROM files
WHERE user_id = sqlc.arg(user_id)
  AND id = sqlc.arg(id)
  AND copy_requested_at IS NOT NULL
  AND storage_key IS NOT NULL;

-- name: ListStaleCopies :many
-- The copy roots marked more than retry_hours ago, per user. Their copy job can have failed for good.
SELECT user_id, array_agg(id ORDER BY id)::bigint[] AS root_ids
FROM files
WHERE copy_requested_at < NOW() - make_interval(hours => sqlc.arg(retry_hours)::int)
GROUP BY user_id
ORDER BY user_id;

-- name: FinishCopies :exec
-- Clears the copying mark, so the copy roots work like any other item.
UPDATE files
SET copy_requested_at = NULL
WHERE user_id = sqlc.arg(user_id)
  AND id = ANY(sqlc.arg(ids)::bigint[]);

-- name: ListLiveItems :many
-- The live items in ids, without locks. A zip request checks them, and the zip job reads them again when it starts.
SELECT * FROM files
WHERE user_id = sqlc.arg(user_id)
  AND id = ANY(sqlc.arg(ids)::bigint[])
  AND trashed_at IS NULL
ORDER BY id;

-- name: SetHidden :many
-- Hides the user's live items in ids, or shows them again when hidden is false. An item already in that state keeps
-- it, so a hidden item keeps its first time. It returns each id it found, and whether it changed, for the audit log.
WITH found AS (
    SELECT i.id, i.hidden_at FROM files i
    WHERE i.user_id = sqlc.arg(user_id)
      AND i.id = ANY(sqlc.arg(ids)::bigint[])
      AND i.trashed_at IS NULL
    -- In id order, like the other locks, so two requests at once cannot deadlock.
    ORDER BY i.id
    FOR UPDATE
), changed AS (
    UPDATE files f
    SET hidden_at = CASE WHEN sqlc.arg(hidden)::boolean THEN NOW() END
    FROM found
    WHERE f.id = found.id
      AND (found.hidden_at IS NULL) = sqlc.arg(hidden)::boolean
    RETURNING f.id
)
SELECT found.id, (changed.id IS NOT NULL)::boolean AS changed
FROM found
LEFT JOIN changed ON changed.id = found.id
ORDER BY found.id;
