-- +goose Up
-- The trash sweep finds the groups marked for a permanent delete long ago. Only a few rows hold the mark.
-- It finds the items in the trash longer than 30 days through idx_files_trash_roots, which holds trashed_at.
CREATE INDEX idx_files_purge_requested ON files(purge_requested_at) WHERE purge_requested_at IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_files_purge_requested;
