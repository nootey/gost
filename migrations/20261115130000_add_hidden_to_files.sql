-- +goose Up
-- hidden_at marks an item the user hid. The list leaves it out unless the user asks for hidden items. A zip keeps it.
ALTER TABLE files ADD COLUMN hidden_at TIMESTAMP;

-- +goose Down
ALTER TABLE files DROP COLUMN IF EXISTS hidden_at;
