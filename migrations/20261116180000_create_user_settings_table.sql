-- +goose Up
-- The user's own settings, one row per user. The first save makes the row. A user with no row has the defaults
-- in the user domain, so the columns have no defaults of their own.
CREATE TABLE user_settings (
    user_id BIGINT PRIMARY KEY REFERENCES users(id),
    sort_favorites_first BOOLEAN NOT NULL,
    sort_folders_first BOOLEAN NOT NULL,
    show_hidden BOOLEAN NOT NULL,
    show_extensions BOOLEAN NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);

-- +goose Down
DROP TABLE IF EXISTS user_settings;
