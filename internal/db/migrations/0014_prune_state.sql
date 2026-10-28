-- +goose Up
-- How much a sync has pruned under each prefix since its baseline, so a cap
-- on deletions is measured against the last full listing rather than the
-- shrinking store, and cannot be walked down a half at a time.
CREATE TABLE prune_state (
    prefix   TEXT PRIMARY KEY,
    baseline INTEGER NOT NULL,       -- stored docs when the window began
    pruned   INTEGER NOT NULL DEFAULT 0,
    since    INTEGER NOT NULL        -- unix seconds the window began
);
-- An operator's confirmation that the next prune of a source or docs owner
-- may exceed the cap; consumed by that prune.
CREATE TABLE prune_confirm (
    key TEXT PRIMARY KEY,            -- "source|<id>" or "docs|<owner>"
    at  INTEGER NOT NULL
);

-- +goose Down
DROP TABLE prune_confirm;
DROP TABLE prune_state;
