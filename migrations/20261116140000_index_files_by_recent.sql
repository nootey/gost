-- +goose Up
-- The recents page reads the user's newest live files from all folders. Folders and the trash are not in the index,
-- so the page reads its few rows from the top of it.
CREATE INDEX idx_files_recent ON files(user_id, updated_at, id) WHERE storage_key IS NOT NULL AND trashed_at IS NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_files_recent;
