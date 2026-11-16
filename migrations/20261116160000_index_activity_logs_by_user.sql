-- +goose Up
-- The activity page lists all of one user's events, newest first. The record index puts record_id before id, so it
-- cannot serve that order.
CREATE INDEX idx_activity_logs_user ON activity_logs(user_id, category, id);

-- +goose Down
DROP INDEX IF EXISTS idx_activity_logs_user;
