-- +goose Up
-- One pin per agent session, written once at SessionStart. Every column but
-- ended_at is fixed for the session's life (INV-05); a pin outlives its
-- session so the command log's retention can count from ended_at.
CREATE TABLE session_pins (
    session_id      TEXT PRIMARY KEY,
    harness         TEXT NOT NULL,    -- claude | codex, read by orchd
    harness_pid     INTEGER NOT NULL,
    harness_started INTEGER NOT NULL, -- unix nanoseconds; with the PID names one process
    first_cwd       TEXT NOT NULL,
    main_checkout   TEXT NOT NULL,    -- '' outside a git checkout
    space           TEXT NOT NULL,    -- '' with no spaces policy applied
    policy_revision INTEGER NOT NULL, -- 0 with no spaces policy applied
    pinned_at       TEXT NOT NULL,    -- RFC 3339 UTC
    ended_at        TEXT,             -- RFC 3339 UTC; NULL while not ended
    -- The session whose pin this one copied: a subagent's parent, or an
    -- earlier session in the same harness process. '' for its own pin.
    inherited_from  TEXT NOT NULL DEFAULT ''
);

CREATE INDEX session_pins_harness ON session_pins (harness_pid, harness_started);

-- +goose Down
DROP TABLE session_pins;
