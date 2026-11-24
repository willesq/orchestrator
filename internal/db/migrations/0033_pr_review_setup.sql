-- +goose Up
-- How a PR's reviews are set up: where the project's instructions come from,
-- what the diff is taken against, and whether the repo's hooks and MCP
-- servers run ('' is automatic). workspace is the running review's checkout
-- directory, removed once its task is done.
ALTER TABLE pr_reviews ADD COLUMN instructions_from TEXT NOT NULL DEFAULT 'default';
ALTER TABLE pr_reviews ADD COLUMN compare_against TEXT NOT NULL DEFAULT 'base';
ALTER TABLE pr_reviews ADD COLUMN project_hooks TEXT NOT NULL DEFAULT '';
ALTER TABLE pr_reviews ADD COLUMN workspace TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE pr_reviews DROP COLUMN workspace;
ALTER TABLE pr_reviews DROP COLUMN project_hooks;
ALTER TABLE pr_reviews DROP COLUMN compare_against;
ALTER TABLE pr_reviews DROP COLUMN instructions_from;
