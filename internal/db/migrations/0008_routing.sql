-- +goose Up
-- routing ranks, per task kind, the models a dispatch of that kind should use.
-- Rank 1 is preferred; later ranks are fallbacks when an earlier one is no
-- longer routable or its provider cannot run in the worker. pinned rows were
-- set by hand and survive `orch routing apply`.
CREATE TABLE IF NOT EXISTS routing (
    task_kind  TEXT NOT NULL,
    rank       INTEGER NOT NULL CHECK (rank >= 1),
    provider   TEXT NOT NULL,
    model      TEXT NOT NULL,
    effort     TEXT NOT NULL DEFAULT '',
    rationale  TEXT NOT NULL DEFAULT '',
    evidence_n INTEGER NOT NULL DEFAULT 0,
    decided_at INTEGER NOT NULL,
    pinned     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (task_kind, rank)
);

-- routing_history keeps each replaced table, one generation per change, so a
-- change can be rolled back.
CREATE TABLE IF NOT EXISTS routing_history (
    generation INTEGER NOT NULL,
    task_kind  TEXT NOT NULL,
    rank       INTEGER NOT NULL,
    provider   TEXT NOT NULL,
    model      TEXT NOT NULL,
    effort     TEXT NOT NULL DEFAULT '',
    rationale  TEXT NOT NULL DEFAULT '',
    evidence_n INTEGER NOT NULL DEFAULT 0,
    decided_at INTEGER NOT NULL,
    pinned     INTEGER NOT NULL DEFAULT 0,
    replaced_at INTEGER NOT NULL,
    PRIMARY KEY (generation, task_kind, rank)
);

-- The route each task ran with, so outcomes can be attributed to it.
ALTER TABLE tasks ADD COLUMN task_kind TEXT;
ALTER TABLE tasks ADD COLUMN provider  TEXT;
ALTER TABLE tasks ADD COLUMN model     TEXT;
ALTER TABLE tasks ADD COLUMN effort    TEXT;

-- +goose Down
ALTER TABLE tasks DROP COLUMN effort;
ALTER TABLE tasks DROP COLUMN model;
ALTER TABLE tasks DROP COLUMN provider;
ALTER TABLE tasks DROP COLUMN task_kind;
DROP TABLE routing_history;
DROP TABLE routing;
