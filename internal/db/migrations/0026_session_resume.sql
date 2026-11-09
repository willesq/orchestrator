-- +goose Up
-- A resumed session (SessionStart source=resume in a new harness process)
-- moves its pin to the new process: harness_pid and harness_started change
-- and ended_at is cleared, while space and first_cwd stay fixed (INV-05).
-- resumed_at is when that last happened; NULL for a session never resumed.
ALTER TABLE session_pins ADD COLUMN resumed_at TEXT;

-- +goose Down
ALTER TABLE session_pins DROP COLUMN resumed_at;
