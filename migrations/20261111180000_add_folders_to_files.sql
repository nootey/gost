-- +goose Up
-- Folders live in the files table: a folder is a row with no storage_key, so it has no bytes on disk.
ALTER TABLE files
    ADD COLUMN parent_id BIGINT REFERENCES files(id),
    ALTER COLUMN storage_key DROP NOT NULL;

-- Names were not unique before, so number the extra copies the way the API does: "a.txt" -> "a (1).txt".
-- Each copy takes the lowest number whose name is still free, so it never clashes with a name already in use.
WITH copies AS (
    SELECT id, user_id, name,
           ROW_NUMBER() OVER (PARTITION BY user_id, name ORDER BY id) - 1 AS n
    FROM files
),
clashes AS (
    SELECT user_id, name,
           COALESCE(substring(name FROM '^(.+)\.[^.]+$'), name) AS base,
           COALESCE(substring(name FROM '^.+(\.[^.]+)$'), '') AS ext
    FROM copies
    WHERE n = 1
),
free_names AS (
    SELECT c.user_id, c.name, candidate.name AS free_name,
           ROW_NUMBER() OVER (PARTITION BY c.user_id, c.name ORDER BY k) AS n
    FROM clashes c
    CROSS JOIN LATERAL generate_series(
        1, (SELECT COUNT(*) FROM files f WHERE f.user_id = c.user_id AND starts_with(f.name, c.base))
    ) AS k
    CROSS JOIN LATERAL (SELECT c.base || ' (' || k || ')' || c.ext AS name) AS candidate
    WHERE NOT EXISTS (SELECT 1 FROM files f WHERE f.user_id = c.user_id AND f.name = candidate.name)
)
UPDATE files f
SET name = fn.free_name
FROM copies c
JOIN free_names fn ON fn.user_id = c.user_id AND fn.name = c.name AND fn.n = c.n
WHERE f.id = c.id;

-- Root items have no parent, so COALESCE puts them in one group for the unique check.
DROP INDEX IF EXISTS idx_files_user_id_created_at;
CREATE UNIQUE INDEX idx_files_user_parent_name ON files(user_id, COALESCE(parent_id, 0), name);
CREATE INDEX idx_files_user_parent_listing ON files(user_id, COALESCE(parent_id, 0), (storage_key IS NULL) DESC, created_at DESC, id DESC);

-- +goose Down
-- Going down loses the folder tree: files move back to one flat list and folders are deleted.
DROP INDEX IF EXISTS idx_files_user_parent_listing;
DROP INDEX IF EXISTS idx_files_user_parent_name;
ALTER TABLE files DROP COLUMN parent_id;
DELETE FROM files WHERE storage_key IS NULL;
ALTER TABLE files ALTER COLUMN storage_key SET NOT NULL;
CREATE INDEX idx_files_user_id_created_at ON files(user_id, created_at DESC, id DESC);
