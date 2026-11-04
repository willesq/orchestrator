-- +goose Up
-- An owner can be paused without forgetting it, and can cover only some repos.
ALTER TABLE docs_owners ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1;
ALTER TABLE docs_owners ADD COLUMN repo_mode TEXT NOT NULL DEFAULT 'all'; -- all | include | exclude
ALTER TABLE docs_owners ADD COLUMN repos TEXT NOT NULL DEFAULT ''; -- comma-separated repo names, without the owner

-- +goose Down
ALTER TABLE docs_owners DROP COLUMN repos;
ALTER TABLE docs_owners DROP COLUMN repo_mode;
ALTER TABLE docs_owners DROP COLUMN enabled;
