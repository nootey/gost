-- +goose Up
-- Finds every row of a trash group, for restore and purge. Only rows in the trash are in it.
CREATE INDEX idx_files_trash_groups ON files(trash_root_id) WHERE trash_root_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_files_trash_groups;
