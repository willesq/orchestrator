-- +goose Up
-- watched_at is when the current watch began; a watch stops at its time limit
-- and restarts the clock when the user watches the PR again.
ALTER TABLE pr_reviews ADD COLUMN watched_at INTEGER NOT NULL DEFAULT 0;
UPDATE pr_reviews SET watched_at = created_at;

-- +goose Down
ALTER TABLE pr_reviews DROP COLUMN watched_at;
