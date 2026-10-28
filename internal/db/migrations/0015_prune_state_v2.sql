-- +goose Up
-- The prune cap's state, replacing whatever shape 0014 left (draft builds of
-- it differed). It only bounds future prunes, so starting empty is safe: the
-- next walk of each prefix takes its baseline from what is stored.
DROP TABLE IF EXISTS prune_confirm;
DROP TABLE IF EXISTS prune_state;
CREATE TABLE prune_state (
    prefix     TEXT PRIMARY KEY,
    key        TEXT NOT NULL DEFAULT '',   -- "source|<id>" or "docs|<owner>": who confirms it
    baseline   INTEGER NOT NULL,           -- docs stored at the last clean walk or confirmed prune
    removed    INTEGER NOT NULL DEFAULT 0, -- docs removed since then without a confirmation
    refused    INTEGER NOT NULL DEFAULT 0, -- docs the latest refused prune would remove; 0 = none waiting
    refused_at INTEGER NOT NULL DEFAULT 0  -- unix seconds of that refusal
);
-- One-shot, short-lived grants for one refused prune of one prefix.
CREATE TABLE prune_confirm (
    prefix  TEXT PRIMARY KEY,
    at      INTEGER NOT NULL,              -- unix seconds granted; expires after an hour
    refused INTEGER NOT NULL               -- the refused prune it was granted for
);

-- +goose Down
DROP TABLE IF EXISTS prune_confirm;
DROP TABLE IF EXISTS prune_state;
