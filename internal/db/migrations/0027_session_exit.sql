-- +goose Up
-- exited_at is when orchd first saw a pin's harness process gone, set by the
-- liveness sweep (SP-017) also for a pin SessionEnd ended first. Command log
-- retention (SP-015) counts from the later of ended_at and exited_at, so a
-- session ended early while its process kept running gets its full
-- retention after the process exits. NULL while the process runs, and for
-- pins ended before this column existed until the next sweep.
ALTER TABLE session_pins ADD COLUMN exited_at TEXT;

-- +goose Down
ALTER TABLE session_pins DROP COLUMN exited_at;
