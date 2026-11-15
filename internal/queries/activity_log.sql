-- name: LogItems :exec
-- Adds one "file" audit row per item in ids. The metadata holds the item's name and its folder's name as they are
-- now (a null folder is Home), merged with extra. So a caller runs it after the change.
INSERT INTO activity_logs (user_id, category, record_id, event, metadata)
SELECT f.user_id, 'file', f.id, sqlc.arg(event)::text,
       jsonb_build_object('name', f.name, 'folder', p.name) || sqlc.arg(extra)::jsonb
FROM files f
LEFT JOIN files p ON p.id = f.parent_id
WHERE f.user_id = sqlc.arg(user_id)
  AND f.id = ANY(sqlc.arg(ids)::bigint[])
ORDER BY f.id;

-- name: LogMoves :exec
-- Adds a "moved" audit row per item in ids. It runs before the move, so "from" is the folder each item leaves and
-- "folder" the one it goes to. A parent_id of 0 means the root, and a null folder is Home.
INSERT INTO activity_logs (user_id, category, record_id, event, metadata)
SELECT f.user_id, 'file', f.id, 'moved', jsonb_build_object('name', f.name, 'from', op.name, 'folder', np.name)
FROM files f
LEFT JOIN files op ON op.id = f.parent_id
LEFT JOIN files np ON np.id = NULLIF(sqlc.arg(parent_id)::bigint, 0)
WHERE f.user_id = sqlc.arg(user_id)
  AND f.id = ANY(sqlc.arg(ids)::bigint[])
ORDER BY f.id;
