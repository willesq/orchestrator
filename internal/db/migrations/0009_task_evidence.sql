-- +goose Up
-- Outcome labels routing learns from. rating is the user's own verdict;
-- pr_outcome is read from the fate of the PRs a task opened. Both are
-- good | rework | bad, and empty until known.
ALTER TABLE tasks ADD COLUMN rating        TEXT;
ALTER TABLE tasks ADD COLUMN rated_at      INTEGER;
ALTER TABLE tasks ADD COLUMN pr_outcome    TEXT;
ALTER TABLE tasks ADD COLUMN pr_checked_at INTEGER;
-- created_at (unix seconds) bounds which PRs a task can claim: only ones opened
-- after it was dispatched.
ALTER TABLE tasks ADD COLUMN created_at    INTEGER;

-- +goose Down
ALTER TABLE tasks DROP COLUMN created_at;
ALTER TABLE tasks DROP COLUMN pr_checked_at;
ALTER TABLE tasks DROP COLUMN pr_outcome;
ALTER TABLE tasks DROP COLUMN rated_at;
ALTER TABLE tasks DROP COLUMN rating;
