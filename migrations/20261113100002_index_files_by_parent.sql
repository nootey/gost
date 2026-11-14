-- +goose Up
-- The trash job finds the rows under a folder by parent_id. A purge needs it too, because Postgres checks
-- the parent_id foreign key for each deleted row. The list indexes do not serve parent_id = $1.
CREATE INDEX idx_files_parent ON files(parent_id);

-- +goose Down
DROP INDEX IF EXISTS idx_files_parent;
