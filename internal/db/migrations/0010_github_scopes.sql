-- +goose Up
-- github_scopes is what the GitHub watcher monitors: the user's own account and
-- the orgs they belong to, each opted in or out. Rows are created as the user's
-- identity is discovered (their account enabled, orgs disabled) and are never
-- overwritten by discovery afterwards, so the user's choices stick.
CREATE TABLE IF NOT EXISTS github_scopes (
    owner      TEXT PRIMARY KEY COLLATE NOCASE,   -- GitHub login (user or org); GitHub logins are case-insensitive
    kind       TEXT NOT NULL,                     -- user | org
    enabled    INTEGER NOT NULL DEFAULT 0,
    items      TEXT NOT NULL DEFAULT 'involving', -- involving | all
    repo_mode  TEXT NOT NULL DEFAULT 'all',       -- all | include | exclude
    repos      TEXT NOT NULL DEFAULT '',          -- comma-separated repo names, without the owner
    updated_at INTEGER NOT NULL DEFAULT 0         -- unix seconds
);

-- +goose Down
DROP TABLE IF EXISTS github_scopes;
