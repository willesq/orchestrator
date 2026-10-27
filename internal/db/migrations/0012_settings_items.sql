-- +goose Up
-- settings_items is the desired state of every setting orch manages in another
-- tool's files: Claude Code hooks, permission rules and plugins, cc-safety-net
-- rules, and whatever providers are added later. A row is keyed by provider,
-- kind and a stable identity, so a toggle survives the file being rewritten.
-- The full payload is kept, so an item turned off (and so removed from its
-- file) is restored exactly when it is turned back on. State is per machine:
-- each machine's orch.db holds that machine's choices, and nothing syncs them.
CREATE TABLE IF NOT EXISTS settings_items (
    provider   TEXT NOT NULL,              -- e.g. claude-code, cc-safety-net
    kind       TEXT NOT NULL,              -- e.g. hook, permission, plugin, rule
    identity   TEXT NOT NULL,              -- stable within provider and kind
    enabled    INTEGER NOT NULL DEFAULT 1, -- desired state
    origin     TEXT NOT NULL,              -- orch | you | found
    payload    TEXT NOT NULL,              -- JSON the provider writes back
    present    INTEGER NOT NULL DEFAULT 0, -- seen in the file at the last look
    removed_at INTEGER NOT NULL DEFAULT 0, -- unix seconds it vanished from the file outside orch; 0 = not
    created_at INTEGER NOT NULL DEFAULT 0, -- unix seconds
    updated_at INTEGER NOT NULL DEFAULT 0, -- unix seconds
    PRIMARY KEY (provider, kind, identity)
);

-- +goose Down
DROP TABLE IF EXISTS settings_items;
