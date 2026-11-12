-- +goose Up
-- One index per sort field. A list query reads one part (folders or files) in one direction,
-- so a backward scan of the same index serves the descending sort.
DROP INDEX IF EXISTS idx_files_user_parent_listing;
CREATE INDEX idx_files_list_by_name ON files(user_id, COALESCE(parent_id, 0), (storage_key IS NULL), lower(name), id);
CREATE INDEX idx_files_list_by_size ON files(user_id, COALESCE(parent_id, 0), (storage_key IS NULL), size_bytes, lower(name), id);
CREATE INDEX idx_files_list_by_type ON files(user_id, COALESCE(parent_id, 0), (storage_key IS NULL), content_type, lower(name), id);
CREATE INDEX idx_files_list_by_date ON files(user_id, COALESCE(parent_id, 0), (storage_key IS NULL), created_at, id);

-- +goose Down
DROP INDEX IF EXISTS idx_files_list_by_date;
DROP INDEX IF EXISTS idx_files_list_by_type;
DROP INDEX IF EXISTS idx_files_list_by_size;
DROP INDEX IF EXISTS idx_files_list_by_name;
CREATE INDEX idx_files_user_parent_listing ON files(user_id, COALESCE(parent_id, 0), (storage_key IS NULL) DESC, created_at DESC, id DESC);
