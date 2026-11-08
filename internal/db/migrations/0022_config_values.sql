-- +goose Up
-- orch's own settings. Only values that differ from the compiled default are
-- stored, so a default changed in a later release reaches everyone who never
-- set it; deleting a row resets the key.
CREATE TABLE config_values (
    key        TEXT PRIMARY KEY,  -- dotted key from config.Fields
    value      TEXT NOT NULL,     -- JSON, in the shape config.ParseJSON accepts
    updated_at TEXT NOT NULL,     -- RFC 3339 UTC
    updated_by TEXT NOT NULL      -- app, import, orchd:<what>, cli:<command>
);

-- The one-time import of a config.yaml: a row here means it ran, so it never
-- runs twice even though the file is left in place.
CREATE TABLE config_import (
    path        TEXT NOT NULL,
    sha256      TEXT NOT NULL,
    imported_at TEXT NOT NULL
);

-- +goose Down
DROP TABLE config_import;
DROP TABLE config_values;
