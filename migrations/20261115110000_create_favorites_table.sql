-- +goose Up
-- A favorite marks one of the user's items. A purge deletes the item's row, and its favorite goes with it.
-- The key starts with file_id, so it serves the lookup of one page of items and the delete that cascades from files.
CREATE TABLE favorites (
    file_id BIGINT NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    user_id BIGINT NOT NULL REFERENCES users(id),
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    PRIMARY KEY (file_id, user_id)
);

-- +goose Down
DROP TABLE IF EXISTS favorites;
