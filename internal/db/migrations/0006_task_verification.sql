-- +goose Up
-- How the work behind a task was verified before it was recorded as complete:
-- "passed: …", "failed: …" or "skipped: …".
ALTER TABLE tasks ADD COLUMN verification TEXT;

-- +goose Down
ALTER TABLE tasks DROP COLUMN verification;
