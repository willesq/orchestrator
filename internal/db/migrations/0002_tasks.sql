-- +goose Up
-- tasks records the terminal result of dispatched agent-task workflows. The
-- durable workflow state itself lives in workflows.db (go-workflows); this table
-- is the human-inspectable result ledger.
CREATE TABLE IF NOT EXISTS tasks (
    id         TEXT PRIMARY KEY,
    agent      TEXT NOT NULL,
    body       TEXT NOT NULL,
    result     TEXT,
    status     TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

-- +goose Down
DROP TABLE tasks;
