-- +goose Up
-- Files sessions and queued tasks wrote outside every folder root (spaces
-- SP-029, INV-09): one row per path, writer space and the hash of the text
-- the write put there, so a later ingest of that same text from that path
-- is placed in that space (quarantine when writers of two spaces wrote the
-- same text). Recorded from the Write tool calls orchd resolves to a pinned
-- session or a tracked task; rows expire.
CREATE TABLE session_files (
    path         TEXT NOT NULL,  -- cleaned, symlinks resolved, lower case
    space        TEXT NOT NULL,  -- the writer's space
    content_hash TEXT NOT NULL,  -- rag.ContentHash of the redacted text written
    writer       TEXT NOT NULL,  -- session:<id> | task:<id>, the latest writer
    written_at   TEXT NOT NULL,  -- RFC 3339 UTC of the latest write
    PRIMARY KEY (path, space, content_hash)
);

CREATE INDEX session_files_written ON session_files (written_at);

-- +goose Down
DROP TABLE session_files;
