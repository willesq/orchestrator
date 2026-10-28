-- +goose Up
-- Superseded by 0015, which replaces both tables (draft builds of this
-- branch shipped other shapes of them under this version).
-- The prune cap's state per repo prefix: what a complete walk last found
-- stored there, and a refused prune waiting for the operator.
CREATE TABLE prune_state (
    prefix   TEXT PRIMARY KEY,
    key      TEXT NOT NULL DEFAULT '',  -- "source|<id>" or "docs|<owner>": who confirms it
    baseline INTEGER NOT NULL,          -- docs stored after the last complete walk
    refused  INTEGER NOT NULL DEFAULT 0 -- docs a refused prune would remove; 0 = none waiting
);
-- One-shot, short-lived grants letting a refused prune of one prefix through.
CREATE TABLE prune_confirm (
    prefix TEXT PRIMARY KEY,
    at     INTEGER NOT NULL             -- unix seconds granted; expires after an hour
);

-- +goose Down
DROP TABLE IF EXISTS prune_confirm;
DROP TABLE IF EXISTS prune_state;
