-- +goose Up
-- downloaded_at is set the first time the user gets a ready zip. The client counts the ready zips not downloaded yet.
ALTER TABLE downloads ADD COLUMN downloaded_at TIMESTAMP;

-- The downloads list reads one user's newest rows. It also serves the foreign key on user_id.
CREATE INDEX idx_downloads_user ON downloads(user_id, id);

-- +goose Down
DROP INDEX IF EXISTS idx_downloads_user;
ALTER TABLE downloads DROP COLUMN IF EXISTS downloaded_at;
