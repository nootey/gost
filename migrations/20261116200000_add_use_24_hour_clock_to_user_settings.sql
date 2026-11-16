-- +goose Up
-- The default only fills the rows that exist now. The user domain keeps the defaults, so the column drops it after.
ALTER TABLE user_settings ADD COLUMN use_24_hour_clock BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE user_settings ALTER COLUMN use_24_hour_clock DROP DEFAULT;

-- +goose Down
ALTER TABLE user_settings DROP COLUMN IF EXISTS use_24_hour_clock;
