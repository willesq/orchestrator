-- +goose Up
-- The enforcement switches (spaces SP-070): one row per layer the operator
-- has switched. A layer with no row is at its default. A timed switch holds
-- the state it reverts to and when.
CREATE TABLE spaces_enforcement (
    layer    TEXT PRIMARY KEY,   -- memory | guard | terms | sandbox | hooks
    state    TEXT NOT NULL,
    set_at   TEXT NOT NULL,      -- RFC 3339 UTC
    set_by   TEXT NOT NULL,      -- the caller orchd identified
    until    TEXT,               -- RFC 3339 UTC; NULL when untimed
    previous TEXT NOT NULL DEFAULT '' -- the state a timed switch reverts to
);

-- Every change, newest last, with its time and caller (INV-16). Never
-- updated or deleted by orchd.
CREATE TABLE spaces_enforcement_log (
    seq      INTEGER PRIMARY KEY AUTOINCREMENT,
    at       TEXT NOT NULL,      -- RFC 3339 UTC
    layer    TEXT NOT NULL,
    state    TEXT NOT NULL,
    previous TEXT NOT NULL,
    by       TEXT NOT NULL,      -- the caller, or 'timer' for an expiry
    until    TEXT,
    reverts_to TEXT NOT NULL DEFAULT '', -- a timed switch's revert state
    expired  INTEGER NOT NULL DEFAULT 0
);

-- +goose Down
DROP TABLE spaces_enforcement_log;
DROP TABLE spaces_enforcement;
