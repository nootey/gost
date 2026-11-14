-- name: LockUserDownloads :exec
-- Makes one user's zip requests run one at a time, so two at once cannot both pass the limit on building zips.
-- FOR NO KEY UPDATE does not block the foreign key checks of rows that point at the user.
SELECT id FROM users
WHERE id = sqlc.arg(user_id)
FOR NO KEY UPDATE;

-- name: CountBuildingDownloads :one
SELECT COUNT(*) FROM downloads
WHERE user_id = sqlc.arg(user_id)
  AND ready_at IS NULL
  AND error IS NULL;

-- name: CreateDownload :one
INSERT INTO downloads (user_id, name, item_ids)
VALUES (sqlc.arg(user_id), sqlc.arg(name), sqlc.arg(item_ids)::bigint[])
    RETURNING *;

-- name: GetDownload :one
SELECT * FROM downloads
WHERE id = sqlc.arg(id) AND user_id = sqlc.arg(user_id);

-- name: FinishDownload :execrows
-- Marks a zip ready. No row changes when the zip failed or its row is gone, and the caller then removes the zip.
UPDATE downloads
SET storage_key = sqlc.arg(storage_key), size_bytes = sqlc.arg(size_bytes), ready_at = NOW()
WHERE id = sqlc.arg(id)
  AND user_id = sqlc.arg(user_id)
  AND ready_at IS NULL
  AND error IS NULL;

-- name: FailDownload :exec
-- Marks a zip that is still building as failed, so the client stops waiting for it.
UPDATE downloads
SET error = sqlc.arg(error)
WHERE id = sqlc.arg(id)
  AND user_id = sqlc.arg(user_id)
  AND ready_at IS NULL
  AND error IS NULL;

-- name: DeleteExpiredDownloads :many
-- Deletes up to batch_size downloads older than retention_hours: a ready zip from when it got ready, any other from
-- when it was asked for. A row still building that old lost its job for good. It returns the keys of their zips.
WITH batch AS (
    SELECT id FROM downloads
    WHERE COALESCE(ready_at, created_at) < NOW() - make_interval(hours => sqlc.arg(retention_hours)::int)
    ORDER BY id
    LIMIT sqlc.arg(batch_size)
)
DELETE FROM downloads
USING batch
WHERE downloads.id = batch.id
RETURNING downloads.id, downloads.storage_key;

-- name: ListUnknownZipKeys :many
-- The names in keys that no download has as its zip.
SELECT k.key::text FROM unnest(sqlc.arg(keys)::text[]) AS k(key)
WHERE NOT EXISTS (SELECT 1 FROM downloads d WHERE d.storage_key = k.key);
