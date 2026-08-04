-- +goose Up
-- sources is the unified registry of everything ingestable on a cadence: web
-- URLs, individual repos, and owners (a GitHub org/user whose repos' markdown is
-- ingested). One freshness scheduler walks this table: refresh-on-cadence, and on
-- absence flag status='missing' while retaining existing chunks.
CREATE TABLE IF NOT EXISTS sources (
    id           TEXT PRIMARY KEY,   -- "<kind>|<uri>"
    kind         TEXT NOT NULL,      -- web | repo | owner
    uri          TEXT NOT NULL,
    scope        TEXT NOT NULL DEFAULT '',
    tags         TEXT NOT NULL DEFAULT '', -- comma-separated
    owner        TEXT NOT NULL DEFAULT '',
    refresh_secs INTEGER NOT NULL DEFAULT 86400,
    last_fetched INTEGER NOT NULL DEFAULT 0, -- unix seconds
    status       TEXT NOT NULL DEFAULT 'new'
);

-- +goose Down
DROP TABLE sources;
