-- +goose Up
-- pr_landings records what the user decided about the commit a task proposed
-- for a PR. A task with no row here has not been decided.
CREATE TABLE IF NOT EXISTS pr_landings (
    task_id     TEXT PRIMARY KEY,
    url         TEXT NOT NULL,              -- https://github.com/<owner>/<repo>/pull/<n>
    branch      TEXT NOT NULL DEFAULT '',   -- the PR's head branch
    path        TEXT NOT NULL DEFAULT '',   -- the worktree the proposal came from
    sha         TEXT NOT NULL,              -- the proposed commit
    prior_head  TEXT NOT NULL DEFAULT '',   -- the PR's head when the user decided
    revert_sha  TEXT NOT NULL DEFAULT '',   -- the commit an undo pushed
    state       TEXT NOT NULL,              -- landed | undone | discarded
    created_at  INTEGER NOT NULL DEFAULT 0, -- unix seconds
    updated_at  INTEGER NOT NULL DEFAULT 0
);

-- +goose Down
DROP TABLE IF EXISTS pr_landings;
