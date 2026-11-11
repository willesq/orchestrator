-- +goose Up
-- Copies of the command log's anchored chain heads and pruned heads (spaces
-- SP-073, D-35). toollog.db keeps each chain's anchors and the head of
-- retention's last cut next to its records, so someone who can rewrite that
-- file can rewrite those too; orchd writes every anchor and every cut here
-- as well, and `orch spaces log verify` compares the two.
CREATE TABLE toollog_anchors (
    owner_kind TEXT    NOT NULL,
    owner_id   TEXT    NOT NULL,
    seq        INTEGER NOT NULL,
    hash       TEXT    NOT NULL,
    at         INTEGER NOT NULL, -- the end record's time, unix nanoseconds
    PRIMARY KEY (owner_kind, owner_id, seq)
);

-- The pruned head of each chain retention cut: records up to seq may be
-- gone, and hash is the record at seq's.
CREATE TABLE toollog_cuts (
    owner_kind TEXT    NOT NULL,
    owner_id   TEXT    NOT NULL,
    seq        INTEGER NOT NULL,
    hash       TEXT    NOT NULL,
    PRIMARY KEY (owner_kind, owner_id)
);

-- One row once orchd first copied the pruned heads toollog.db held before
-- these tables existed, so that copy runs once.
CREATE TABLE toollog_copy_started (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    at TEXT    NOT NULL
);

-- +goose Down
DROP TABLE toollog_copy_started;
DROP TABLE toollog_cuts;
DROP TABLE toollog_anchors;
