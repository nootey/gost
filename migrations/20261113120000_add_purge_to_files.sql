-- +goose Up
-- A permanent delete marks the items the user deleted. The trash view and restore skip a marked group,
-- and a job deletes its rows in short batches. Only a trash root can hold the mark.
ALTER TABLE files
    ADD COLUMN purge_requested_at TIMESTAMP,
    ADD CONSTRAINT files_purge_check CHECK (purge_requested_at IS NULL OR trash_root_id = id);

-- +goose Down
ALTER TABLE files
    DROP CONSTRAINT files_purge_check,
    DROP COLUMN purge_requested_at;
