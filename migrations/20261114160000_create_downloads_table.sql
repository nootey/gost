-- +goose Up
-- A zip download of a folder or many items. A job builds the zip, then sets storage_key and ready_at.
-- error is set when the build fails for good. A row with neither is still building.
-- storage_key names the zip in the zips dir under the upload dir.
CREATE TABLE downloads (
    id BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id),
    name TEXT NOT NULL,
    item_ids BIGINT[] NOT NULL,
    storage_key VARCHAR(64) UNIQUE,
    size_bytes BIGINT NOT NULL DEFAULT 0,
    error TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    ready_at TIMESTAMP,
    CONSTRAINT downloads_ready_check CHECK ((ready_at IS NULL) = (storage_key IS NULL)),
    CONSTRAINT downloads_state_check CHECK (ready_at IS NULL OR error IS NULL)
);

-- A request counts the user's zips that still build.
CREATE INDEX idx_downloads_building ON downloads(user_id) WHERE ready_at IS NULL AND error IS NULL;

-- The sweep finds old downloads by age: a ready zip from when it got ready, any other from when it was asked for.
CREATE INDEX idx_downloads_age ON downloads((COALESCE(ready_at, created_at)));

-- +goose Down
DROP TABLE IF EXISTS downloads;
