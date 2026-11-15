-- +goose Up
-- The operator's retags (spaces SP-071, D-14): each row is a sticky
-- override that places every later write of source_uri in space, and the
-- state of the move that took its stored chunks there. 'moving' is written
-- before the move starts and 'done' after it finished, so orchd resumes a
-- move an exit interrupted. A retag of the same source again replaces the
-- row.
CREATE TABLE spaces_retags (
    source_uri TEXT PRIMARY KEY,
    space      TEXT NOT NULL,      -- the space the operator named
    state      TEXT NOT NULL,      -- moving | done
    held_in    TEXT NOT NULL DEFAULT '', -- comma-separated spaces that held it
    confirmed  INTEGER NOT NULL DEFAULT 0, -- 1: the operator confirmed the move
    at         TEXT NOT NULL,      -- RFC 3339 UTC of the last change
    by         TEXT NOT NULL       -- the caller orchd identified
);

-- +goose Down
DROP TABLE spaces_retags;
