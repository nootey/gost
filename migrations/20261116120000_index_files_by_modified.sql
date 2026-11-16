-- +goose Up
-- The file list sorts by the modified time too. Like the other list indexes, one index serves both directions.
CREATE INDEX idx_files_list_by_modified ON files(user_id, COALESCE(parent_id, 0), (storage_key IS NULL), updated_at, id);

-- +goose Down
DROP INDEX IF EXISTS idx_files_list_by_modified;
