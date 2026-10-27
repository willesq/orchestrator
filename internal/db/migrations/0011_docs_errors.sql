-- +goose Up
-- Why a docs sync failed, kept with the owner and each repo so the failure can
-- be shown (orch status, the app) instead of only an "error" status.
ALTER TABLE docs_owners ADD COLUMN error TEXT NOT NULL DEFAULT '';
ALTER TABLE docs_repos ADD COLUMN error TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE docs_repos DROP COLUMN error;
ALTER TABLE docs_owners DROP COLUMN error;
