-- +goose Up
-- A paste adds the copy roots at once, and a job adds the rows under them. copied_from_id is the row a copy came from,
-- so the job finds the rows it made already. No foreign key: a purge can delete the source.
-- copy_requested_at marks a copy root until the job ends. Only a copy can hold the mark.
ALTER TABLE files
    ADD COLUMN copied_from_id BIGINT,
    ADD COLUMN copy_requested_at TIMESTAMP,
    ADD CONSTRAINT files_copy_check CHECK (copy_requested_at IS NULL OR copied_from_id IS NOT NULL);

-- For each source row, the job asks if its copy is in the copy folder yet. A source has few copies, so the job
-- reads them by source and checks the folder.
CREATE INDEX idx_files_copied_from ON files(copied_from_id) WHERE copied_from_id IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS idx_files_copied_from;

ALTER TABLE files
    DROP CONSTRAINT files_copy_check,
    DROP COLUMN copy_requested_at,
    DROP COLUMN copied_from_id;
