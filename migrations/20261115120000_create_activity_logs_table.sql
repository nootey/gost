-- +goose Up
-- The audit log. Each row is one event on one record of the user, such as a rename of a file. category names the kind
-- of record ("file"), and record_id its id. record_id has no foreign key, so the log stays after a purge.
-- metadata holds the values of the event as they were then, such as the item's name and its folder's name.
CREATE TABLE activity_logs (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id),
    category VARCHAR(32) NOT NULL,
    record_id BIGINT NOT NULL,
    event VARCHAR(32) NOT NULL,
    metadata JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMP NOT NULL DEFAULT NOW()
);

-- One record's log, newest first. It also serves the foreign key on user_id.
CREATE INDEX idx_activity_logs_record ON activity_logs(user_id, category, record_id, id);

-- +goose Down
DROP TABLE IF EXISTS activity_logs;
