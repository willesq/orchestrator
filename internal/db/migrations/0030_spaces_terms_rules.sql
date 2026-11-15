-- +goose Up
-- The operator's outbound rules (spaces SP-075): every change to the
-- outbound terms check's rules, newest last. Each row holds the whole rule
-- set after the change, so the newest row is what applies and a replay can
-- judge an event with the rules in force when it was recorded. Never
-- updated or deleted by orchd. 'rules' holds the operator's rule values,
-- which never leave orch.db and the guard snapshot (INV-07); the other
-- columns name a rule by its id only.
CREATE TABLE spaces_terms_rules (
    rev     INTEGER PRIMARY KEY AUTOINCREMENT,
    at      TEXT NOT NULL,      -- RFC 3339 UTC, nanoseconds
    by      TEXT NOT NULL,      -- the caller orchd identified
    action  TEXT NOT NULL,      -- enable | disable | add | remove
    rule_id TEXT NOT NULL,      -- a baseline rule id or user-<n>
    rules   TEXT NOT NULL,      -- JSON of spaces.OutboundRules after the change
    hash    TEXT NOT NULL       -- SHA-256 of rules
);

-- +goose Down
DROP TABLE spaces_terms_rules;
