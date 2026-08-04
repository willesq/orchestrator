-- +goose Up
-- meta holds small key/value bookkeeping (schema version marker, asset version, etc.).
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- +goose Down
DROP TABLE meta;
