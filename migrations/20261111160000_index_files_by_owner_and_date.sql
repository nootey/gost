-- +goose Up
DROP INDEX IF EXISTS idx_files_user_id;
CREATE INDEX idx_files_user_id_created_at ON files(user_id, created_at DESC, id DESC);

-- +goose Down
DROP INDEX IF EXISTS idx_files_user_id_created_at;
CREATE INDEX idx_files_user_id ON files(user_id);
