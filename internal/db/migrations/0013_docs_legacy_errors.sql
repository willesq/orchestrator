-- +goose Up
-- Failures recorded before 0011 added the error columns have status "error" and
-- no reason, which status and the app showed as a blank failure. Give them one;
-- the next sync of the repo or owner replaces it.
UPDATE docs_repos SET error = 'failed before failure reasons were recorded; the next sync retries it'
WHERE status = 'error' AND error = '';
UPDATE docs_owners SET error = 'failed before failure reasons were recorded; the next sync retries it'
WHERE status = 'error' AND error = '';

-- +goose Down
UPDATE docs_repos SET error = '' WHERE error = 'failed before failure reasons were recorded; the next sync retries it';
UPDATE docs_owners SET error = '' WHERE error = 'failed before failure reasons were recorded; the next sync retries it';
