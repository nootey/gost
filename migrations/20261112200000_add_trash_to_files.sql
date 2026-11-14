-- +goose Up
-- A delete marks the item and every row under it. trash_root_id is the item the user deleted.
-- So each row knows by itself that it is in the trash, and a list needs no walk up the parents.
ALTER TABLE files
    ADD COLUMN trashed_at TIMESTAMP,
    ADD COLUMN trash_root_id BIGINT,
    ADD CONSTRAINT files_trash_check CHECK ((trashed_at IS NULL) = (trash_root_id IS NULL));

-- Items in the trash hold no name, and the lists skip them.
DROP INDEX IF EXISTS idx_files_user_parent_name;
DROP INDEX IF EXISTS idx_files_list_by_name;
DROP INDEX IF EXISTS idx_files_list_by_size;
DROP INDEX IF EXISTS idx_files_list_by_type;
DROP INDEX IF EXISTS idx_files_list_by_date;
CREATE UNIQUE INDEX idx_files_user_parent_name ON files(user_id, COALESCE(parent_id, 0), name) WHERE trashed_at IS NULL;
CREATE INDEX idx_files_list_by_name ON files(user_id, COALESCE(parent_id, 0), (storage_key IS NULL), lower(name), id) WHERE trashed_at IS NULL;
CREATE INDEX idx_files_list_by_size ON files(user_id, COALESCE(parent_id, 0), (storage_key IS NULL), size_bytes, lower(name), id) WHERE trashed_at IS NULL;
CREATE INDEX idx_files_list_by_type ON files(user_id, COALESCE(parent_id, 0), (storage_key IS NULL), content_type, lower(name), id) WHERE trashed_at IS NULL;
CREATE INDEX idx_files_list_by_date ON files(user_id, COALESCE(parent_id, 0), (storage_key IS NULL), created_at, id) WHERE trashed_at IS NULL;

-- +goose Down
-- Going down empties the trash. The bytes of those files stay on disk.
DELETE FROM files WHERE trashed_at IS NOT NULL;

DROP INDEX IF EXISTS idx_files_list_by_date;
DROP INDEX IF EXISTS idx_files_list_by_type;
DROP INDEX IF EXISTS idx_files_list_by_size;
DROP INDEX IF EXISTS idx_files_list_by_name;
DROP INDEX IF EXISTS idx_files_user_parent_name;
CREATE UNIQUE INDEX idx_files_user_parent_name ON files(user_id, COALESCE(parent_id, 0), name);
CREATE INDEX idx_files_list_by_name ON files(user_id, COALESCE(parent_id, 0), (storage_key IS NULL), lower(name), id);
CREATE INDEX idx_files_list_by_size ON files(user_id, COALESCE(parent_id, 0), (storage_key IS NULL), size_bytes, lower(name), id);
CREATE INDEX idx_files_list_by_type ON files(user_id, COALESCE(parent_id, 0), (storage_key IS NULL), content_type, lower(name), id);
CREATE INDEX idx_files_list_by_date ON files(user_id, COALESCE(parent_id, 0), (storage_key IS NULL), created_at, id);

ALTER TABLE files
    DROP CONSTRAINT files_trash_check,
    DROP COLUMN trash_root_id,
    DROP COLUMN trashed_at;
