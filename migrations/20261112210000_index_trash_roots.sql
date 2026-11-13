-- +goose Up
-- The trash view lists the items the user deleted. This index serves its default sort, newest first, folders first.
CREATE INDEX idx_files_trash_roots ON files(user_id, (storage_key IS NULL), trashed_at, id) WHERE trash_root_id = id;

-- +goose Down
DROP INDEX IF EXISTS idx_files_trash_roots;
