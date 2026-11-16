-- +goose Up
-- The favorites list reads all of one user's favorites. The primary key starts with file_id, so it cannot serve that.
-- It also serves the foreign key on user_id.
CREATE INDEX idx_favorites_user ON favorites(user_id);

-- +goose Down
DROP INDEX IF EXISTS idx_favorites_user;
