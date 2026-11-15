-- name: AddFavorites :exec
-- Marks the user's live items in ids as favorites. An item that is a favorite already stays as it is.
INSERT INTO favorites (file_id, user_id)
SELECT f.id, f.user_id FROM files f
WHERE f.user_id = sqlc.arg(user_id)
  AND f.id = ANY(sqlc.arg(ids)::bigint[])
  AND f.trashed_at IS NULL
ON CONFLICT DO NOTHING;

-- name: RemoveFavorites :exec
DELETE FROM favorites
WHERE user_id = sqlc.arg(user_id)
  AND file_id = ANY(sqlc.arg(ids)::bigint[]);

-- name: ListFavoriteIDs :many
-- The items in ids that are the user's favorites.
SELECT file_id FROM favorites
WHERE user_id = sqlc.arg(user_id)
  AND file_id = ANY(sqlc.arg(ids)::bigint[]);
