-- +goose Up
-- The spaces policy, one row per saved revision. Saving never changes what is
-- enforced: only an operator's apply does, recorded in spaces_applied.
CREATE TABLE spaces_revisions (
    revision   INTEGER PRIMARY KEY AUTOINCREMENT,
    doc        TEXT NOT NULL,     -- canonical JSON of spaces.Policy
    sha256     TEXT NOT NULL,     -- hex sha256 of doc
    created_at TEXT NOT NULL,     -- RFC 3339 UTC
    created_by TEXT NOT NULL,     -- the caller orchd identified
    -- 1 when the operator saved it; a draft an agent session saved needs an
    -- explicit confirmation to apply.
    by_operator INTEGER NOT NULL DEFAULT 0
);

-- Every apply, newest last. At startup orchd enforces the newest entry when
-- its revision still hashes to the sha256 recorded here, and nothing (failing
-- closed) when it does not, so an edit to orch.db never takes effect; while
-- orchd runs, such an edit shows as drift.
CREATE TABLE spaces_applied (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    revision   INTEGER NOT NULL REFERENCES spaces_revisions(revision),
    sha256     TEXT NOT NULL,
    applied_at TEXT NOT NULL,
    applied_by TEXT NOT NULL
);

-- +goose Down
DROP TABLE spaces_applied;
DROP TABLE spaces_revisions;
