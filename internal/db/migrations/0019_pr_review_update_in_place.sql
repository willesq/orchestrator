-- +goose Up
-- update_in_place has orch edit its earlier comment on the PR rather than add another.
ALTER TABLE pr_reviews ADD COLUMN update_in_place INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE pr_reviews DROP COLUMN update_in_place;
