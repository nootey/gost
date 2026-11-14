-- +goose Up
-- The copy sweep finds the copy roots marked long ago. Only a few rows hold the mark.
CREATE INDEX idx_files_copy_requested ON files(copy_requested_at) WHERE copy_requested_at IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_files_copy_requested;
