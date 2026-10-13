-- +goose Up
-- Outcome metrics for each finished task, so the ledger shows what a run cost
-- and which models did the work. That is the evidence model routing will need.
-- models and prs are comma-separated lists.
ALTER TABLE tasks ADD COLUMN cost_usd    REAL;
ALTER TABLE tasks ADD COLUMN duration_ms INTEGER;
ALTER TABLE tasks ADD COLUMN turns       INTEGER;
ALTER TABLE tasks ADD COLUMN session_id  TEXT;
ALTER TABLE tasks ADD COLUMN models      TEXT;
ALTER TABLE tasks ADD COLUMN prs         TEXT;

-- +goose Down
ALTER TABLE tasks DROP COLUMN prs;
ALTER TABLE tasks DROP COLUMN models;
ALTER TABLE tasks DROP COLUMN session_id;
ALTER TABLE tasks DROP COLUMN turns;
ALTER TABLE tasks DROP COLUMN duration_ms;
ALTER TABLE tasks DROP COLUMN cost_usd;
