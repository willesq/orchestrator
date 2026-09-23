-- +goose Up
-- docs is the GitHub code-docs subsystem (distinct from `sources`): a registered
-- GitHub owner (user/org) whose repos' markdown is cloned + ingested on a cadence.
-- Its own scheduler walks docs_owners (sleep-safe: due-ness from last_synced).
CREATE TABLE IF NOT EXISTS docs_owners (
    owner        TEXT PRIMARY KEY,        -- GitHub login (user or org)
    refresh_secs INTEGER NOT NULL DEFAULT 21600, -- 6h
    scope        TEXT NOT NULL DEFAULT '',
    tags         TEXT NOT NULL DEFAULT '', -- comma-separated
    last_synced  INTEGER NOT NULL DEFAULT 0, -- unix seconds
    status       TEXT NOT NULL DEFAULT 'new'
);

-- Per-repo sync bookkeeping: the last-synced HEAD SHA lets a sync skip a repo
-- entirely when its default branch hasn't moved (no clone, no re-ingest).
CREATE TABLE IF NOT EXISTS docs_repos (
    owner       TEXT NOT NULL,
    repo        TEXT NOT NULL,
    head_sha    TEXT NOT NULL DEFAULT '',
    last_synced INTEGER NOT NULL DEFAULT 0,
    files       INTEGER NOT NULL DEFAULT 0,
    status      TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (owner, repo)
);

-- Per-file content hash: skip embedding a markdown file whose content is unchanged.
CREATE TABLE IF NOT EXISTS doc_hashes (
    uri        TEXT PRIMARY KEY, -- repo://<owner>/<repo>/<path>
    hash       TEXT NOT NULL,
    updated_at INTEGER NOT NULL DEFAULT 0
);

-- +goose Down
DROP TABLE doc_hashes;
DROP TABLE docs_repos;
DROP TABLE docs_owners;
